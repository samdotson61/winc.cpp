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
	"regexp"
	"strconv"
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
	rp.ModifyResponse = rewriteContextOverflowError
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && isChatPath(req.URL.Path) {
			if body, rerr := io.ReadAll(req.Body); rerr == nil {
				req.Body.Close()
				cb, _, _ := compactRequest(body, nil) // lossless minify only (no tool stripping in single mode)
				// Thinking is rewritten only in adaptive mode; on/off/fixed are set by the
				// server's own flags, so pass those through untouched. The router still runs
				// in every mode (minify + the context-overflow rewrite below apply always).
				nb := cb
				if cfg.Reasoning.Mode == "adaptive" {
					nb = injectThinking(cfg, cb)
				}
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

// llamaCtxOverflow matches llama-server's context-overflow message as a fallback for when
// the structured fields are absent: "request (N tokens) exceeds the available context size
// (M tokens)".
var llamaCtxOverflow = regexp.MustCompile(`\((\d+)\s*tokens?\)\s*exceeds the available context size\s*\((\d+)\s*tokens?\)`)

// rewriteContextOverflowError translates llama-server's context-overflow error into the
// exact wording Claude Code recognizes. llama-server says "...exceeds the available context
// size...", but Claude Code only detects an over-long prompt when the error message contains
// the literal "prompt is too long: N tokens > M maximum" -- used by BOTH the main loop's
// context handling AND auto mode's command-safety classifier. Without this, an overflow
// (common when a large bash tool_result balloons the request on a small local context) is
// misread as the model being DOWN: auto mode reports "<model> is temporarily unavailable, so
// auto mode cannot determine the safety of <command>" and blocks the command (fail-closed),
// instead of compacting and retrying. We rewrite ONLY this specific error; every other
// response -- including success streams -- passes through untouched (we return immediately
// for any status < 400, so SSE bodies are never buffered).
func rewriteContextOverflowError(resp *http.Response) error {
	if resp == nil || resp.StatusCode < 400 {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return nil // compressed; leave as-is
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	restore := func() error { resp.Body = io.NopCloser(bytes.NewReader(body)); return nil }
	if err != nil {
		return restore()
	}
	var le struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			NPrompt int    `json:"n_prompt_tokens"`
			NCtx    int    `json:"n_ctx"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &le) != nil {
		return restore()
	}
	isOverflow := le.Error.Type == "exceed_context_size_error" ||
		strings.Contains(le.Error.Message, "exceeds the available context size")
	if !isOverflow {
		return restore()
	}
	actual, limit := le.Error.NPrompt, le.Error.NCtx
	if actual == 0 || limit == 0 {
		if m := llamaCtxOverflow.FindStringSubmatch(le.Error.Message); m != nil {
			actual, _ = strconv.Atoi(m[1])
			limit, _ = strconv.Atoi(m[2])
		}
	}
	if actual == 0 || limit == 0 {
		return restore() // couldn't extract counts; don't fabricate numbers
	}
	msg := fmt.Sprintf("prompt is too long: %d tokens > %d maximum", actual, limit)
	// Encode WITHOUT HTML-escaping so the ">" stays literal (json.Marshal would emit
	// ">"); harmless after JSON-decode, but a literal ">" is unambiguous on the wire.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "invalid_request_error", "message": msg},
	}) != nil {
		return restore()
	}
	nb := bytes.TrimRight(buf.Bytes(), "\n")
	resp.StatusCode = http.StatusBadRequest
	resp.Status = "400 Bad Request"
	resp.Body = io.NopCloser(bytes.NewReader(nb))
	resp.ContentLength = int64(len(nb))
	resp.Header.Set("Content-Length", strconv.Itoa(len(nb)))
	resp.Header.Set("Content-Type", "application/json")
	return nil
}

// Route maps an exact on-the-wire model string to a backend, with a per-route
// thinking policy: "" = adaptive (default), "low" = a small fixed budget (small
// models orchestrate tools best with a LITTLE thinking), "off" = disabled.
type Route struct {
	Model    string   // the resolved model id Claude Code sends (winc's tier alias)
	Upstream string   // backend base URL
	Think    string   // "" | "low" | "off"
	Tools    []string // tool allowlist for this backend ("" / nil = keep all; ["all"] = keep all)
}

// Tier is one rung of the subagent escalation ladder (ascending capability). A subagent
// request tagged with the ladder's model is routed to the first rung whose MaxEstTokens
// covers its estimated load; the last rung is the catch-all. Lets subagents START small
// and escalate by the degree of load, deterministically (infra-driven, no model judgment).
type Tier struct {
	Upstream     string
	Think        string
	MaxEstTokens int      // route here when estimated load <= this; last rung = catch-all
	Tools        []string // tool allowlist for this rung (nil / ["all"] = keep all)
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
		rp.ModifyResponse = rewriteContextOverflowError
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
				var allow []string // tool allowlist for the chosen worker backend (nil = HEAD, keep all)
				if ladderTag != "" && model == ladderTag && len(ladder) > 0 {
					t := pickTier(ladder, body)
					target, think, allow = t.Upstream, t.Think, t.Tools
				} else if rt, ok := byModel[model]; ok {
					target, think, allow = rt.Upstream, rt.Think, rt.Tools
				}
				cb, toolsBefore, toolsAfter := compactRequest(body, allow)
				nb := injectThinkingPolicy(cfg, cb, think)
				req.Body = io.NopCloser(bytes.NewReader(nb))
				req.ContentLength = int64(len(nb))
				req.Header.Set("Content-Length", fmt.Sprintf("%d", len(nb)))
				req.Header.Del("Accept-Encoding")
				if rlog != nil {
					rlog.Printf("model=%q -> %s%s%s", model, target, thinkTag(think), toolsTag(toolsBefore, toolsAfter))
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

// compactRequest shrinks a chat request for a local model: strips the tools array to a
// per-backend allowlist (worker tiers) and losslessly minifies it. allow nil/empty or
// ["all"] = keep all tools (the HEAD model). Returns the new body and the tool counts
// before/after (for the router log). Deterministic per (body, allow), so a stripped worker
// prefix stays byte-stable and llama.cpp's prefix cache still reuses it.
func compactRequest(body []byte, allow []string) (out []byte, toolsBefore, toolsAfter int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, 0, 0
	}
	if arr, ok := toolsArray(m); ok {
		toolsBefore = len(arr)
	}
	changed := false
	if len(allow) > 0 && !containsFold(allow, "all") {
		changed = stripToolsToAllowlist(m, allow) || changed
	}
	changed = minifyRequest(m) || changed
	if arr, ok := toolsArray(m); ok {
		toolsAfter = len(arr)
	} else {
		toolsAfter = toolsBefore
	}
	if !changed {
		return body, toolsBefore, toolsAfter
	}
	if nb, err := json.Marshal(m); err == nil {
		return nb, toolsBefore, toolsAfter
	}
	return body, toolsBefore, toolsAfter
}

func toolsArray(m map[string]json.RawMessage) ([]json.RawMessage, bool) {
	raw, ok := m["tools"]
	if !ok {
		return nil, false
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil, false
	}
	return arr, true
}

func containsFold(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

// stripToolsToAllowlist filters the tools array to those whose name is in allow, preserving
// order. No-op if nothing matches (never hand a worker an empty tool set) or nothing is
// removed -- so it's safe and deterministic.
func stripToolsToAllowlist(m map[string]json.RawMessage, allow []string) bool {
	arr, ok := toolsArray(m)
	if !ok || len(arr) == 0 {
		return false
	}
	keep := map[string]bool{}
	for _, a := range allow {
		keep[strings.ToLower(strings.TrimSpace(a))] = true
	}
	out := make([]json.RawMessage, 0, len(arr))
	for _, t := range arr {
		var nm struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &nm)
		if keep[strings.ToLower(nm.Name)] {
			out = append(out, t)
		}
	}
	if len(out) == 0 || len(out) == len(arr) {
		return false
	}
	nb, err := json.Marshal(out)
	if err != nil {
		return false
	}
	m["tools"] = nb
	return true
}

// minifyRequest losslessly trims the tools array: drops the optional input_examples field
// from each tool. Names / descriptions / input_schema are required and left intact.
func minifyRequest(m map[string]json.RawMessage) bool {
	arr, ok := toolsArray(m)
	if !ok || len(arr) == 0 {
		return false
	}
	changed := false
	for i, t := range arr {
		var obj map[string]json.RawMessage
		if json.Unmarshal(t, &obj) != nil {
			continue
		}
		if _, has := obj["input_examples"]; has {
			delete(obj, "input_examples")
			if nb, err := json.Marshal(obj); err == nil {
				arr[i] = nb
				changed = true
			}
		}
	}
	if !changed {
		return false
	}
	if nb, err := json.Marshal(arr); err == nil {
		m["tools"] = nb
		return true
	}
	return false
}

func toolsTag(before, after int) string {
	if before != after {
		return fmt.Sprintf(" tools=%d->%d", before, after)
	}
	return ""
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
