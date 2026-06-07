// Package router is winc-router: a tiny in-process reverse proxy used only in
// adaptive reasoning mode. It reads each chat request, injects a per-request
// thinking budget (or disables thinking), and streams the response through
// unchanged. All local, Python-free, sub-millisecond overhead.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"winc/internal/config"
	"winc/internal/reasoning"
)

type Router struct {
	srv  *http.Server
	ln   net.Listener
	base string
}

// Start launches the router in front of upstream (the llama-server/-swap URL) on
// an ephemeral localhost port.
func Start(cfg *config.Config, upstream string) (*Router, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	// Claude Code routinely cancels in-flight requests (Esc, abandoned background
	// calls, early SSE close, client timeouts). Go's default proxy ErrorHandler logs
	// "http: proxy error: context canceled" to stderr -- which, since winc shares the
	// terminal with the agent, prints into Claude Code's chat box. Swallow expected
	// client cancellations silently; surface only genuine upstream failures (502).
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
			return // client hung up on purpose -- nothing to report
		}
		w.WriteHeader(http.StatusBadGateway)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && isChatPath(req.URL.Path) {
			if body, rerr := io.ReadAll(req.Body); rerr == nil {
				req.Body.Close()
				nb := injectThinking(cfg, body)
				req.Body = io.NopCloser(bytes.NewReader(nb))
				req.ContentLength = int64(len(nb))
				req.Header.Set("Content-Length", fmt.Sprintf("%d", len(nb)))
				req.Header.Del("Accept-Encoding")
			}
		}
		rp.ServeHTTP(w, req)
	})
	// Send any remaining server-internal log lines to the void rather than the shared
	// terminal, so nothing from winc ever corrupts the agent's TUI.
	srv := &http.Server{Handler: mux, ErrorLog: log.New(io.Discard, "", 0)}
	r := &Router{srv: srv, ln: ln, base: "http://" + ln.Addr().String()}
	go srv.Serve(ln)
	return r, nil
}

// BaseURL is the local URL the agent should point ANTHROPIC_BASE_URL at.
func (r *Router) BaseURL() string { return r.base }

// Stop shuts the router down.
func (r *Router) Stop() {
	if r != nil && r.srv != nil {
		_ = r.srv.Close()
	}
}

func isChatPath(p string) bool {
	return strings.HasSuffix(p, "/v1/messages") || strings.HasSuffix(p, "/v1/chat/completions")
}

func injectThinking(cfg *config.Config, body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body // not an object we understand; pass through untouched
	}
	d := reasoning.Decide(cfg, body)
	if !d.Set {
		return body
	}
	if d.EnableThinking && d.BudgetTokens > 0 {
		think, _ := json.Marshal(map[string]any{"type": "enabled", "budget_tokens": d.BudgetTokens})
		m["thinking"] = think
		ensureMaxTokens(m, d.BudgetTokens)
	} else {
		m["chat_template_kwargs"] = mergeKwargs(m["chat_template_kwargs"], "enable_thinking", false)
	}
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return body
}

// ensureMaxTokens guarantees room for the answer beyond the thinking budget.
func ensureMaxTokens(m map[string]json.RawMessage, budget int) {
	floor := budget + 512
	if raw, ok := m["max_tokens"]; ok {
		var cur int
		if json.Unmarshal(raw, &cur) == nil && cur >= floor {
			return
		}
	}
	v, _ := json.Marshal(floor)
	m["max_tokens"] = v
}

func mergeKwargs(existing json.RawMessage, key string, val any) json.RawMessage {
	obj := map[string]any{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &obj)
	}
	obj[key] = val
	out, _ := json.Marshal(obj)
	return out
}
