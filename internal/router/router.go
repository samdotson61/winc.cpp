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
	"sort"
	"strconv"
	"strings"
	"sync"

	"winc/internal/config"
	"winc/internal/paths"
	"winc/internal/reasoning"
)

type Router struct {
	srv     *http.Server
	ln      net.Listener
	base    string
	logf    *os.File // team-mode routing log (nil otherwise)
	logPath string   // "" when the routing log could not be created

	mu         sync.Mutex
	rlog       *log.Logger
	requests   map[string]int  // chat requests routed, by backend name
	dead       map[string]bool // upstreams the watchdog marked dead
	deadNames  []string        // human names of dead backends (for stats)
	overflows  int             // context-overflow errors rewritten for the agent
	capsLow    int             // requests whose max_tokens was lowered to a tier cap
	deadSkips  int             // requests re-routed past a dead backend
	infoPinned int             // information-only requests held on a worker instead of the head model
}

// fallbackName is the stats/log name for the fallback backend (the main model).
const fallbackName = "main"

// Start launches the router in front of upstream (the llama-server/-swap URL) on
// an ephemeral localhost port. ctxWindow, when > 0, is the server's loaded context
// size -- used to trim an oversized compaction request so its summary can complete.
func Start(cfg *config.Config, upstream string, ctxWindow int) (*Router, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	r := &Router{requests: map[string]int{}, dead: map[string]bool{}}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ErrorHandler = swallowClientCancel
	rp.ModifyResponse = r.rewriteOverflow
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && isChatPath(req.URL.Path) {
			if body, rerr := io.ReadAll(req.Body); rerr == nil {
				req.Body.Close()
				nb := body
				// One parse for the whole rewrite pipeline (see preq); an unparseable
				// body passes through untouched.
				if p := parseReq(body); p != nil {
					p.trimCompaction(ctxWindow)
					p.compact(nil) // lossless minify only (no tool stripping in single mode)
					// Thinking is rewritten only in adaptive mode; on/off/fixed are set by the
					// server's own flags, so pass those through untouched. The router still runs
					// in every mode (minify + the context-overflow rewrite below apply always).
					if cfg.Reasoning.Mode == "adaptive" {
						p.injectThinking(cfg)
					}
					nb = p.encode()
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
	r.srv, r.ln, r.base = srv, ln, "http://"+ln.Addr().String()
	go srv.Serve(ln)
	return r, nil
}

// BaseURL is the local URL the agent should point ANTHROPIC_BASE_URL at.
func (r *Router) BaseURL() string { return r.base }

// LogPath is the routing log's path. "" means the log file could not be created
// (routing still works; only the diagnostics file is missing) or single mode.
func (r *Router) LogPath() string {
	if r == nil {
		return ""
	}
	return r.logPath
}

// Stop shuts the router down, writing the session stats to the routing log first.
func (r *Router) Stop() {
	if r == nil {
		return
	}
	if r.srv != nil {
		_ = r.srv.Close()
	}
	r.mu.Lock()
	logf, rlog := r.logf, r.rlog
	r.logf, r.rlog = nil, nil
	r.mu.Unlock()
	if rlog != nil {
		rlog.Printf("session stats: %s", r.Stats())
	}
	if logf != nil {
		_ = logf.Close()
	}
}

// Stats is a snapshot of the router's session counters. Team mode fills Requests
// per backend name; in single mode only Overflows is meaningful.
type Stats struct {
	Requests    map[string]int // chat requests routed, by backend name
	Dead        []string       // backends the watchdog marked dead (sorted)
	Overflows   int            // context-overflow errors rewritten for the agent
	CapsLowered int            // requests whose max_tokens was lowered to a tier cap
	DeadSkips   int            // requests re-routed past a dead backend
	InfoPinned  int            // information-only requests held on a worker instead of the head model
}

// String renders a compact one-line summary, backends sorted by name.
func (s Stats) String() string {
	var b strings.Builder
	names := make([]string, 0, len(s.Requests))
	for n := range s.Requests {
		names = append(names, n)
	}
	sort.Strings(names)
	b.WriteString("requests:")
	if len(names) == 0 {
		b.WriteString(" none")
	}
	for _, n := range names {
		fmt.Fprintf(&b, " %s=%d", n, s.Requests[n])
	}
	if s.Overflows > 0 {
		fmt.Fprintf(&b, "  overflows-rewritten=%d", s.Overflows)
	}
	if s.CapsLowered > 0 {
		fmt.Fprintf(&b, "  caps-lowered=%d", s.CapsLowered)
	}
	if s.InfoPinned > 0 {
		fmt.Fprintf(&b, "  info-pinned=%d", s.InfoPinned)
	}
	if s.DeadSkips > 0 {
		fmt.Fprintf(&b, "  dead-skips=%d", s.DeadSkips)
	}
	if len(s.Dead) > 0 {
		fmt.Fprintf(&b, "  dead: %s", strings.Join(s.Dead, ","))
	}
	return b.String()
}

// Stats returns a snapshot of the session counters. Safe on a nil Router.
func (r *Router) Stats() Stats {
	s := Stats{Requests: map[string]int{}}
	if r == nil {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.requests {
		s.Requests[k] = v
	}
	s.Dead = append(s.Dead, r.deadNames...)
	sort.Strings(s.Dead)
	s.Overflows, s.CapsLowered, s.DeadSkips, s.InfoPinned = r.overflows, r.capsLow, r.deadSkips, r.infoPinned
	return s
}

// MarkDead records that a backend stopped responding (set by the worker watchdog
// in team mode). Routing skips dead backends from then on: ladder picks re-run
// over the alive rungs and pinned routes fall back to the main model. Detection
// only -- the router never kills or restarts anything.
func (r *Router) MarkDead(name, upstream string) {
	if r == nil || upstream == "" {
		return
	}
	r.mu.Lock()
	if r.dead == nil {
		r.dead = map[string]bool{}
	}
	already := r.dead[upstream]
	if !already {
		r.dead[upstream] = true
		r.deadNames = append(r.deadNames, nameOr(name, upstream))
	}
	rlog := r.rlog
	r.mu.Unlock()
	if !already && rlog != nil {
		rlog.Printf("watchdog: %s (%s) marked dead - rerouting its traffic", nameOr(name, upstream), upstream)
	}
}

func (r *Router) isDead(upstream string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dead[upstream]
}

func (r *Router) note(name string) {
	r.mu.Lock()
	if r.requests == nil {
		r.requests = map[string]int{}
	}
	r.requests[name]++
	r.mu.Unlock()
}

func (r *Router) countDeadSkip() {
	r.mu.Lock()
	r.deadSkips++
	r.mu.Unlock()
}

// nameOr falls back to the upstream URL when a route/tier has no human name.
func nameOr(name, upstream string) string {
	if name != "" {
		return name
	}
	return upstream
}

// rewriteOverflow is the proxy's ModifyResponse hook: it applies the context-
// overflow rewrite and counts each rewrite for the end-of-session stats.
func (r *Router) rewriteOverflow(resp *http.Response) error {
	rewritten, err := rewriteContextOverflowError(resp)
	if rewritten {
		r.mu.Lock()
		r.overflows++
		r.mu.Unlock()
	}
	return err
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
// exact wording Claude Code recognizes, reporting whether it rewrote the response.
// llama-server says "...exceeds the available context size...", but Claude Code only
// detects an over-long prompt when the error message contains the literal "prompt is
// too long: N tokens > M maximum" -- used by BOTH the main loop's context handling AND
// auto mode's command-safety classifier. Without this, an overflow (common when a large
// bash tool_result balloons the request on a small local context) is misread as the
// model being DOWN: auto mode reports "<model> is temporarily unavailable, so auto mode
// cannot determine the safety of <command>" and blocks the command (fail-closed),
// instead of compacting and retrying. We rewrite ONLY this specific error; every other
// response -- including success streams -- passes through untouched (we return
// immediately for any status < 400, so SSE bodies are never buffered).
func rewriteContextOverflowError(resp *http.Response) (bool, error) {
	if resp == nil || resp.StatusCode < 400 {
		return false, nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return false, nil // compressed; leave as-is
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return false, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	restore := func() error { resp.Body = io.NopCloser(bytes.NewReader(body)); return nil }
	if err != nil {
		return false, restore()
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
		return false, restore()
	}
	isOverflow := le.Error.Type == "exceed_context_size_error" ||
		strings.Contains(le.Error.Message, "exceeds the available context size")
	if !isOverflow {
		return false, restore()
	}
	actual, limit := le.Error.NPrompt, le.Error.NCtx
	if actual == 0 || limit == 0 {
		if m := llamaCtxOverflow.FindStringSubmatch(le.Error.Message); m != nil {
			actual, _ = strconv.Atoi(m[1])
			limit, _ = strconv.Atoi(m[2])
		}
	}
	if actual == 0 || limit == 0 {
		return false, restore() // couldn't extract counts; don't fabricate numbers
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
		return false, restore()
	}
	nb := bytes.TrimRight(buf.Bytes(), "\n")
	resp.StatusCode = http.StatusBadRequest
	resp.Status = "400 Bad Request"
	resp.Body = io.NopCloser(bytes.NewReader(nb))
	resp.ContentLength = int64(len(nb))
	resp.Header.Set("Content-Length", strconv.Itoa(len(nb)))
	resp.Header.Set("Content-Type", "application/json")
	return true, nil
}

// Route maps an exact on-the-wire model string to a backend, with a per-route
// thinking policy: "" = adaptive (default), "low" = a small fixed budget (small
// models orchestrate tools best with a LITTLE thinking), "off" = disabled.
type Route struct {
	Name      string   // short human name for logs/stats (e.g. "sonnet"); falls back to Upstream
	Model     string   // the resolved model id Claude Code sends (winc's tier alias)
	Upstream  string   // backend base URL
	Think     string   // "" | "low" | "off"
	Tools     []string // tool allowlist for this backend ("" / nil = keep all; ["all"] = keep all)
	MaxTokens int      // generation cap for this backend (0 = uncapped); lowers an over-large max_tokens
}

// Tier is one rung of the subagent escalation ladder (ascending capability). A subagent
// request tagged with the ladder's model is routed to the first rung whose MaxEstTokens
// covers its estimated load; the last rung is the catch-all. Lets subagents START small
// and escalate by the degree of load, deterministically (infra-driven, no model judgment).
type Tier struct {
	Name         string // short human name for logs/stats (e.g. "haiku"); falls back to Upstream
	Upstream     string
	Think        string
	MaxEstTokens int      // route here when estimated load <= this; last rung = catch-all
	Tools        []string // tool allowlist for this rung (nil / ["all"] = keep all)
	MaxTokens    int      // generation cap for this rung (0 = uncapped); lowers an over-large max_tokens
	Head         bool     // this rung IS the main GPU model; information-only requests never land here
}

// StartTeam launches the team-mode router. It dispatches each chat request by its `model`
// field: the subagent ladderTag escalates across the ladder by load (start small, escalate
// by degree); explicit routes map a model to a fixed backend; anything else (the HEAD) goes
// to fallback. One ANTHROPIC_BASE_URL fronts the main model and its small CPU workers.
// headCtx, when > 0, is the head's per-slot context size -- used to trim an oversized
// compaction request so its summary can complete.
func StartTeam(cfg *config.Config, routes []Route, ladder []Tier, ladderTag, fallback string, headCtx int) (*Router, error) {
	r := &Router{requests: map[string]int{}, dead: map[string]bool{}}
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
		rp.ModifyResponse = r.rewriteOverflow
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
	// and a way to verify each tier reaches the right worker on first run. A failure
	// to create it is surfaced via LogPath() (""), not fatal: routing still works.
	logPath := filepath.Join(paths.InstallDir(), "winc-router.log")
	logf, lerr := os.Create(logPath)
	if lerr != nil {
		logPath, logf = "", nil
	}
	var rlog *log.Logger
	if logf != nil {
		rlog = log.New(logf, "", log.LstdFlags)
	}
	r.logf, r.logPath, r.rlog = logf, logPath, rlog

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		target := fallback
		if req.Method == http.MethodPost && isChatPath(req.URL.Path) {
			if body, rerr := io.ReadAll(req.Body); rerr == nil {
				req.Body.Close()
				// One parse for the whole rewrite pipeline (see preq); an unparseable
				// body is forwarded untouched to the fallback.
				p := parseReq(body)
				model := ""
				if p != nil {
					model = p.model()
					if ladderTag == "" || model != ladderTag {
						// Head-bound request: a compaction that no longer fits the head's
						// window gets its oldest transcript messages dropped so the summary
						// can complete (subagent/ladder requests have their own windows).
						p.trimCompaction(headCtx)
					}
				}
				name := fallbackName
				think := ""
				var allow []string // tool allowlist for the chosen worker backend (nil = HEAD, keep all)
				maxTok := 0        // generation cap for the chosen worker backend (0 = uncapped, e.g. the HEAD)
				if p != nil && ladderTag != "" && model == ladderTag && len(ladder) > 0 {
					if t, ok := r.pickAliveTier(ladder, p); ok {
						target, name, think, allow, maxTok = t.Upstream, nameOr(t.Name, t.Upstream), t.Think, t.Tools, t.MaxTokens
					}
				} else if rt, ok := byModel[model]; ok {
					if r.isDead(rt.Upstream) {
						r.countDeadSkip() // pinned route's backend died -> fall back to main
					} else {
						target, name, think, allow, maxTok = rt.Upstream, nameOr(rt.Name, rt.Upstream), rt.Think, rt.Tools, rt.MaxTokens
					}
				}
				nb := body
				toolsBefore, toolsAfter := 0, 0
				if p != nil {
					toolsBefore, toolsAfter = p.compact(allow)
					p.injectPolicy(cfg, think)
					if p.clamp(maxTok) {
						r.mu.Lock()
						r.capsLow++
						r.mu.Unlock()
					}
					nb = p.encode()
				}
				r.note(name)
				req.Body = io.NopCloser(bytes.NewReader(nb))
				req.ContentLength = int64(len(nb))
				req.Header.Set("Content-Length", fmt.Sprintf("%d", len(nb)))
				req.Header.Del("Accept-Encoding")
				if rlog != nil {
					rlog.Printf("model=%q -> %s (%s)%s%s", model, name, target, thinkTag(think), toolsTag(toolsBefore, toolsAfter))
				}
			}
		}
		proxies[target].ServeHTTP(w, req)
	})
	srv := &http.Server{Handler: mux, ErrorLog: log.New(io.Discard, "", 0)}
	r.srv, r.ln, r.base = srv, ln, "http://"+ln.Addr().String()
	go srv.Serve(ln)
	return r, nil
}

func thinkTag(think string) string {
	if think == "" {
		return ""
	}
	return " (think:" + think + ")"
}

// pickAliveTier picks the load-appropriate rung, skipping rungs whose backend the
// watchdog marked dead: the pick re-runs over only the alive rungs, so requests
// escalate past a dead rung (or settle on the highest alive one when the top died).
// ok=false when every rung is dead -- the caller then uses the fallback backend.
func (r *Router) pickAliveTier(ladder []Tier, p *preq) (Tier, bool) {
	est, heavy := p.estTokens(), reasoning.Heavy(p.body)
	t, pinned := pickTierFrom(ladder, est, heavy, p.infoOnly)
	if r.isDead(t.Upstream) {
		r.countDeadSkip()
		alive := make([]Tier, 0, len(ladder))
		for _, x := range ladder {
			if !r.isDead(x.Upstream) {
				alive = append(alive, x)
			}
		}
		if len(alive) == 0 {
			return Tier{}, false
		}
		t, pinned = pickTierFrom(alive, est, heavy, p.infoOnly)
	}
	if pinned {
		r.mu.Lock()
		r.infoPinned++
		r.mu.Unlock()
	}
	return t, true
}

// pickTier selects an escalation rung by the request's estimated load (start small), then
// bumps one rung for code-heavy requests. The last rung is the catch-all -- EXCEPT for
// information-only requests (read/search/fetch tools, or none), which top out at the
// highest worker rung instead of a Head rung: spinning a second full session on the big
// GPU model just to read and report is strictly slower than a worker, and the request
// carries no tool that could use the head's extra capability. pinned=true when that cap
// changed the pick. Byte-based form of pickTierFrom, kept as the testable contract.
func pickTier(ladder []Tier, body []byte) (Tier, bool) {
	return pickTierFrom(ladder, reasoning.EstimateInputTokens(body), reasoning.Heavy(body),
		func() bool { return infoOnlyRequest(body) })
}

// pickTierFrom is pickTier with the request signals precomputed; infoOnly is lazy
// because only a Head pick needs the tools inspection.
func pickTierFrom(ladder []Tier, est int, heavy bool, infoOnly func() bool) (Tier, bool) {
	t := pickAt(ladder, est, heavy)
	if !t.Head || !infoOnly() {
		return t, false
	}
	workers := make([]Tier, 0, len(ladder))
	for _, x := range ladder {
		if !x.Head {
			workers = append(workers, x)
		}
	}
	if len(workers) == 0 {
		return t, false // degenerate head-only ladder -- nothing to pin to
	}
	return pickAt(workers, est, heavy), true
}

// pickAt is the rung selection: the first rung whose MaxEstTokens covers the estimate
// (last rung = catch-all), bumped one rung for code-heavy requests.
func pickAt(ladder []Tier, est int, heavy bool) Tier {
	idx := len(ladder) - 1
	for i, t := range ladder {
		if est <= t.MaxEstTokens {
			idx = i
			break
		}
	}
	if heavy && idx < len(ladder)-1 {
		idx++
	}
	return ladder[idx]
}

// infoTools is the read/search/fetch toolkit an information-gathering subagent carries
// (Claude Code's read-only tools). A request restricted to these can only look things
// up and report back -- it cannot edit, run commands, or spawn agents.
var infoTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "NotebookRead": true,
	"WebFetch": true, "WebSearch": true, "ToolSearch": true,
	"TaskGet": true, "TaskList": true, "TodoRead": true,
	"ListMcpResourcesTool": true, "ReadMcpResourceTool": true,
	"StructuredOutput": true,
}

// infoOnlyRequest reports whether a chat request is information-only: it carries no
// tools at all (pure read-context-and-report generation), or every tool it carries is
// in the read/search/fetch set. Anything unrecognized (Bash, Edit, Write, MCP tools,
// ...) disqualifies, so a request that can act keeps its right to escalate.
// Byte-based form of (*preq).infoOnly, kept as the testable contract.
func infoOnlyRequest(body []byte) bool {
	p := parseReq(body)
	if p == nil {
		return false
	}
	return p.infoOnly()
}

// preq is one chat request parsed ONCE for the whole rewrite pipeline. The previous
// pipeline re-unmarshaled and re-marshaled the full body at every stage (model
// extraction, compaction trim, tool strip, thinking injection, max_tokens clamp) --
// 5+ decode passes and 3 encode passes over a transcript that grows to megabytes
// late in a session. Stages now mutate the shared top-level map and the body is
// re-encoded exactly once at the end (or passed through byte-identical when no
// stage changed anything, which also keeps llama.cpp's prefix cache stable).
type preq struct {
	m        map[string]json.RawMessage
	body     []byte            // original wire bytes (cheap scans + pass-through)
	estBytes int               // body size for token estimates, net of stripped bytes
	msgs     []json.RawMessage // lazily parsed m["messages"]
	msgsOK   bool
	changed  bool // any field mutated -> re-encode on output
}

// parseReq parses a chat request's top level, or nil when the body isn't an
// object we understand (callers then pass the bytes through untouched).
func parseReq(body []byte) *preq {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	return &preq{m: m, body: body, estBytes: len(body)}
}

// estTokens mirrors reasoning.EstimateInputTokens over the tracked size, so a
// stage that strips tools keeps later estimates honest without re-encoding.
func (p *preq) estTokens() int { return p.estBytes / 4 }

func (p *preq) messages() []json.RawMessage {
	if !p.msgsOK {
		p.msgsOK = true
		_ = json.Unmarshal(p.m["messages"], &p.msgs) // stays nil when absent/malformed
	}
	return p.msgs
}

// model extracts the request's `model` field ("" if absent/unparseable).
func (p *preq) model() string {
	var s string
	_ = json.Unmarshal(p.m["model"], &s)
	return s
}

// encode renders the request: the original bytes when nothing changed, one
// marshal otherwise.
func (p *preq) encode() []byte {
	if !p.changed {
		return p.body
	}
	if nb, err := json.Marshal(p.m); err == nil {
		return nb
	}
	return p.body
}

// infoOnly mirrors infoOnlyRequest over the parsed request.
func (p *preq) infoOnly() bool {
	arr, ok := toolsArray(p.m)
	if !ok || len(arr) == 0 {
		return true
	}
	for _, raw := range arr {
		var t struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &t); err != nil || !infoTools[t.Name] {
			return false
		}
	}
	return true
}

// isCompaction mirrors reasoning.IsCompaction over the parsed request.
func (p *preq) isCompaction() bool {
	msgs := p.messages()
	var last json.RawMessage
	if n := len(msgs); n > 0 {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msgs[n-1], &msg) == nil && msg.Role == "user" {
			last = msg.Content
		}
	}
	return reasoning.CompactionProbe(p.m["system"], last)
}

// summaryRoomTokens is the share of the context window kept free for a compaction
// request's generated summary. A compaction prompt that fills the whole window
// starts the summary and hits the context wall mid-write (truncated output) -- the
// broken summary shrinks nothing and the session loops on the overflow forever.
const summaryRoomTokens = 6144

// trimCompaction makes an oversized compaction request fit its window. Claude Code
// compacts by sending the WHOLE transcript plus a final summarize instruction; when
// the session has already overflowed, that prompt IS the overflow -- it can never
// succeed as sent. Drop the OLDEST messages (the summary cares most about the recent
// state) until the prompt leaves summaryRoomTokens for the summary itself, keeping
// the final instruction and opening the kept transcript on a plain user message (no
// orphaned tool results). Non-compaction requests and compactions that already fit
// pass through untouched. Byte-based form of (*preq).trimCompaction, kept as the
// testable contract.
func trimCompaction(body []byte, window int) []byte {
	p := parseReq(body)
	if p == nil {
		return body
	}
	p.trimCompaction(window)
	return p.encode()
}

func (p *preq) trimCompaction(window int) {
	if window <= 0 || p.estTokens() <= window-summaryRoomTokens {
		return
	}
	if !p.isCompaction() {
		return
	}
	msgs := p.messages()
	if len(msgs) < 3 {
		return
	}
	overBytes := p.estBytes - (window-summaryRoomTokens)*4
	drop := 0
	for drop < len(msgs)-2 && overBytes > 0 {
		overBytes -= len(msgs[drop])
		drop++
	}
	// Never orphan a tool_result: open the kept transcript on a plain user message.
	for drop < len(msgs)-2 && !plainUserMessage(msgs[drop]) {
		drop++
	}
	if drop == 0 {
		return
	}
	dropped := 0
	for _, m := range msgs[:drop] {
		dropped += len(m)
	}
	nb, err := json.Marshal(msgs[drop:])
	if err != nil {
		return
	}
	p.m["messages"] = nb
	p.msgs = msgs[drop:]
	p.estBytes -= dropped
	p.changed = true
}

// plainUserMessage reports whether a message is role=user carrying no tool_result
// blocks -- a clean opening message for a trimmed transcript.
func plainUserMessage(raw json.RawMessage) bool {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
		return false
	}
	return !bytes.Contains(msg.Content, []byte(`"tool_result"`))
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
//
// Byte-based form of (*preq).injectPolicy, kept as the testable contract.
func injectThinkingPolicy(cfg *config.Config, body []byte, think string) []byte {
	p := parseReq(body)
	if p == nil {
		return body
	}
	p.injectPolicy(cfg, think)
	return p.encode()
}

func (p *preq) injectPolicy(cfg *config.Config, think string) {
	switch think {
	case "off":
		delete(p.m, "thinking")
		p.m["chat_template_kwargs"] = mergeKwargs(p.m["chat_template_kwargs"], "enable_thinking", false)
		p.changed = true
	case "low":
		tk, _ := json.Marshal(map[string]any{"type": "enabled", "budget_tokens": lowThinkBudget})
		p.m["thinking"] = tk
		ensureMaxTokens(p.m, lowThinkBudget)
		p.changed = true
	default:
		if cfg.Reasoning.Mode != "adaptive" {
			return
		}
		p.injectThinking(cfg)
	}
}

// compactRequest shrinks a chat request for a local model: strips the tools array to a
// per-backend allowlist (worker tiers) and losslessly minifies it. allow nil/empty or
// ["all"] = keep all tools (the HEAD model). Returns the new body and the tool counts
// before/after (for the router log). Deterministic per (body, allow), so a stripped worker
// prefix stays byte-stable and llama.cpp's prefix cache still reuses it.
// Byte-based form of (*preq).compact, kept as the testable contract.
func compactRequest(body []byte, allow []string) (out []byte, toolsBefore, toolsAfter int) {
	p := parseReq(body)
	if p == nil {
		return body, 0, 0
	}
	toolsBefore, toolsAfter = p.compact(allow)
	return p.encode(), toolsBefore, toolsAfter
}

func (p *preq) compact(allow []string) (toolsBefore, toolsAfter int) {
	rawToolsLen := len(p.m["tools"])
	if arr, ok := toolsArray(p.m); ok {
		toolsBefore = len(arr)
	}
	changed := false
	if len(allow) > 0 && !containsFold(allow, "all") {
		changed = stripToolsToAllowlist(p.m, allow) || changed
	}
	changed = minifyRequest(p.m) || changed
	if arr, ok := toolsArray(p.m); ok {
		toolsAfter = len(arr)
	} else {
		toolsAfter = toolsBefore
	}
	if changed {
		p.estBytes -= rawToolsLen - len(p.m["tools"]) // keep later token estimates honest
		p.changed = true
	}
	return toolsBefore, toolsAfter
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

// injectThinking applies the adaptive thinking decision. The size estimate uses
// the tracked post-strip size; the complexity scan runs over the original bytes
// (scanning is cheap, and stripped tool text could only have bumped the budget
// CEILING up one tier, never down -- the model may always think less).
func (p *preq) injectThinking(cfg *config.Config) {
	d := reasoning.DecideFrom(cfg, p.estTokens(), p.isCompaction(), reasoning.LooksComplex(p.body))
	if !d.Set {
		return
	}
	if d.EnableThinking && d.BudgetTokens > 0 {
		think, _ := json.Marshal(map[string]any{"type": "enabled", "budget_tokens": d.BudgetTokens})
		p.m["thinking"] = think
		ensureMaxTokens(p.m, d.BudgetTokens)
	} else {
		p.m["chat_template_kwargs"] = mergeKwargs(p.m["chat_template_kwargs"], "enable_thinking", false)
	}
	p.changed = true
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

// clampMaxTokens LOWERS an over-large max_tokens to cap for small-worker tiers, so a model
// stuck in a repetition loop can't generate until it slams into its context window (minutes
// of CPU time producing truncated garbage). cap <= 0 is a no-op -- the HEAD model is never
// capped. It only lowers: a request already at/below cap is left untouched; an absent
// max_tokens is set to cap. Runs after the thinking policy, so the thinking floor still
// holds as long as cap exceeds the thinking budget (the defaults do). lowered reports
// whether the request was actually capped (for the session stats).
// Byte-based form of (*preq).clamp, kept as the testable contract.
func clampMaxTokens(body []byte, maxTok int) (out []byte, lowered bool) {
	p := parseReq(body)
	if p == nil {
		return body, false
	}
	lowered = p.clamp(maxTok)
	return p.encode(), lowered
}

func (p *preq) clamp(maxTok int) (lowered bool) {
	if maxTok <= 0 {
		return false
	}
	if raw, ok := p.m["max_tokens"]; ok {
		var cur int
		if json.Unmarshal(raw, &cur) == nil && cur <= maxTok {
			return false // already within the cap -- don't touch
		}
	}
	v, _ := json.Marshal(maxTok)
	p.m["max_tokens"] = v
	p.changed = true
	return true
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
