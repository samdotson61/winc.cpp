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
	"os"
	"path/filepath"
	"strings"

	"winc/internal/config"
	"winc/internal/paths"
	"winc/internal/reasoning"
)

type Router struct {
	srv  *http.Server
	ln   net.Listener
	base string
	logf *os.File // team-mode routing log (nil otherwise)
}

// Start launches the router in front of upstream (the llama-server/-swap URL) on
// an ephemeral localhost port.
func Start(cfg *config.Config, upstream string) (*Router, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ErrorHandler = swallowClientCancel
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
	if r == nil {
		return
	}
	if r.srv != nil {
		_ = r.srv.Close()
	}
	if r.logf != nil {
		_ = r.logf.Close()
		r.logf = nil
	}
}

// swallowClientCancel is the proxy ErrorHandler. Claude Code routinely cancels
// in-flight requests (Esc, abandoned background calls, early SSE close, client
// timeouts); Go's default handler logs "http: proxy error: context canceled" to
// stderr, which -- since winc shares the terminal with the agent -- prints into
// Claude Code's chat box. Swallow expected client cancellations silently; surface
// only genuine upstream failures (502).
func swallowClientCancel(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return // client hung up on purpose -- nothing to report
	}
	w.WriteHeader(http.StatusBadGateway)
}

// Route maps an exact on-the-wire model string to a backend, with a per-route
// thinking policy: "" = adaptive (default), "low" = a small fixed budget (small
// models orchestrate tools best with a LITTLE thinking), "off" = disabled.
type Route struct {
	Model    string // the resolved model id Claude Code sends (winc's tier alias)
	Upstream string // backend base URL
	Think    string // "" | "low" | "off"
}

// Tier is one rung of the subagent escalation ladder (ascending capability). A subagent
// request tagged with the ladder's model is routed to the first rung whose MaxEstTokens
// covers its estimated load; the last rung is the catch-all. Lets subagents START small
// and escalate by the degree of load, deterministically (infra-driven, no model judgment).
type Tier struct {
	Upstream     string
	Think        string
	MaxEstTokens int // route here when estimated load <= this; last rung = catch-all
}

// StartTeam launches the team-mode router. It dispatches each chat request by its `model`
// field: the subagent ladderTag escalates across the ladder by load (start small, escalate
// by degree); explicit routes map a model to a fixed backend; anything else (the HEAD) goes
// to fallback. One ANTHROPIC_BASE_URL fronts the main model and its small CPU workers.
func StartTeam(cfg *config.Config, routes []Route, ladder []Tier, ladderTag, fallback string) (*Router, error) {
	proxies := map[string]*httputil.ReverseProxy{}
	mkProxy := func(target string) error {
		if target == "" || proxies[target] != nil {
			return nil
		}
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		rp := httputil.NewSingleHostReverseProxy(u)
		rp.ErrorHandler = swallowClientCancel
		proxies[target] = rp
		return nil
	}
	if err := mkProxy(fallback); err != nil {
		return nil, err
	}
	byModel := map[string]Route{}
	for _, rt := range routes {
		if rt.Model == "" || rt.Upstream == "" {
			continue
		}
		if err := mkProxy(rt.Upstream); err != nil {
			return nil, err
		}
		byModel[rt.Model] = rt
	}
	for _, t := range ladder {
		if err := mkProxy(t.Upstream); err != nil {
			return nil, err
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// Routing decisions go to a file (never the shared terminal) -- both diagnostics
	// and a way to verify each tier reaches the right worker on first run.
	logf, _ := os.Create(filepath.Join(paths.InstallDir(), "winc-router.log"))
	var rlog *log.Logger
	if logf != nil {
		rlog = log.New(logf, "", log.LstdFlags)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		target := fallback
		if req.Method == http.MethodPost && isChatPath(req.URL.Path) {
			if body, rerr := io.ReadAll(req.Body); rerr == nil {
				req.Body.Close()
				model := modelOf(body)
				think := ""
				if ladderTag != "" && model == ladderTag && len(ladder) > 0 {
					t := pickTier(ladder, body)
					target, think = t.Upstream, t.Think
				} else if rt, ok := byModel[model]; ok {
					target, think = rt.Upstream, rt.Think
				}
				nb := injectThinkingPolicy(cfg, body, think)
				req.Body = io.NopCloser(bytes.NewReader(nb))
				req.ContentLength = int64(len(nb))
				req.Header.Set("Content-Length", fmt.Sprintf("%d", len(nb)))
				req.Header.Del("Accept-Encoding")
				if rlog != nil {
					rlog.Printf("model=%q -> %s%s", model, target, thinkTag(think))
				}
			}
		}
		proxies[target].ServeHTTP(w, req)
	})
	srv := &http.Server{Handler: mux, ErrorLog: log.New(io.Discard, "", 0)}
	r := &Router{srv: srv, ln: ln, base: "http://" + ln.Addr().String(), logf: logf}
	go srv.Serve(ln)
	return r, nil
}

func thinkTag(think string) string {
	if think == "" {
		return ""
	}
	return " (think:" + think + ")"
}

// pickTier selects an escalation rung by the request's estimated load (start small), then
// bumps one rung for code-heavy requests. The last rung is the catch-all.
func pickTier(ladder []Tier, body []byte) Tier {
	est := reasoning.EstimateInputTokens(body)
	idx := len(ladder) - 1
	for i, t := range ladder {
		if est <= t.MaxEstTokens {
			idx = i
			break
		}
	}
	if reasoning.Heavy(body) && idx < len(ladder)-1 {
		idx++
	}
	return ladder[idx]
}

// modelOf extracts the request's `model` field ("" if absent/unparseable).
func modelOf(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// lowThinkBudget is the small thinking allowance for worker tiers. Small models call
// tools far more reliably with a LITTLE thinking than with none, but unbounded thinking
// is slow and can trap the tool call inside the reasoning block -- so keep it tight.
const lowThinkBudget = 512

// injectThinkingPolicy applies a route's thinking policy to a request:
//   - "off": force thinking off (enable_thinking=false).
//   - "low": a small fixed thinking budget -- the sweet spot for small models doing
//     tool use (brief planning, then act), without the latency/trap of full thinking.
//   - "" (adaptive): mirror single mode -- only ADAPTIVE mode rewrites requests; on/off/
//     fixed are set by server flags on each backend, so the request passes through.
func injectThinkingPolicy(cfg *config.Config, body []byte, think string) []byte {
	switch think {
	case "off":
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return body
		}
		delete(m, "thinking")
		m["chat_template_kwargs"] = mergeKwargs(m["chat_template_kwargs"], "enable_thinking", false)
		if out, err := json.Marshal(m); err == nil {
			return out
		}
		return body
	case "low":
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return body
		}
		tk, _ := json.Marshal(map[string]any{"type": "enabled", "budget_tokens": lowThinkBudget})
		m["thinking"] = tk
		ensureMaxTokens(m, lowThinkBudget)
		if out, err := json.Marshal(m); err == nil {
			return out
		}
		return body
	default:
		if cfg.Reasoning.Mode != "adaptive" {
			return body
		}
		return injectThinking(cfg, body)
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
