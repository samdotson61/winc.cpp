package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/reasoning"
)

// A pinned listen address is honored exactly -- the jobdar eval profile's
// contract is a STABLE /v1/messages surface on the winc.toml port.
func TestStartPinnedAddr(t *testing.T) {
	cfg := config.Defaults()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	// Reserve a concrete free port, release it, pin the router there.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	rt, err := Start(&cfg, up.URL, 0, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	if rt.BaseURL() != "http://"+addr {
		t.Fatalf("pinned router URL = %s, want http://%s", rt.BaseURL(), addr)
	}
	// A second router on the SAME address must fail loudly, not fall back.
	if rt2, err2 := Start(&cfg, up.URL, 0, addr); err2 == nil {
		rt2.Stop()
		t.Fatal("second router on a pinned busy address must error")
	}
}

// roundtrip sends body through a router fronting a capturing upstream and returns
// what the upstream received.
func roundtrip(t *testing.T, cfg *config.Config, path, body string) map[string]any {
	t.Helper()
	var captured []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	rt, err := Start(cfg, up.URL, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	resp, err := http.Post(rt.BaseURL()+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var m map[string]any
	if len(captured) > 0 {
		_ = json.Unmarshal(captured, &m)
	}
	return m
}

func TestRouterInjectsBudget(t *testing.T) {
	cfg := config.Defaults()
	m := roundtrip(t, &cfg, "/v1/messages",
		`{"messages":[{"role":"user","content":"write a bunny calculator"}],"max_tokens":50}`)
	if _, ok := m["thinking"]; !ok {
		t.Fatalf("expected thinking injected for complex prompt, got %v", m)
	}
}

// The router must pass `response_format` (json_schema) through to the upstream
// untouched -- the jobdar eval profile's structured-output contract: jobdar
// constrains eval JSON to a schema on /v1/chat/completions, and that constraint
// is meaningless if the router drops it. Covers BOTH router code paths: the
// pass-through (reasoning off -> no rewrite) and the re-encode (adaptive ->
// thinking injected -> the whole preq map is re-marshalled).
func TestResponseFormatPassthrough(t *testing.T) {
	const rf = `"response_format":{"type":"json_schema","json_schema":{"name":"eval","schema":{"type":"object","properties":{"verdict":{"type":"string"}}}}}`

	// (a) eval-representative: reasoning off, a trivial body (no re-encode forced).
	off := config.Defaults()
	off.Reasoning.Mode = "off"
	m := roundtrip(t, &off, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"score this"}],`+rf+`}`)
	if _, ok := m["response_format"]; !ok {
		t.Fatalf("response_format stripped on the pass-through path: %v", m)
	}

	// (b) adaptive + a complex prompt forces the re-encode (thinking injected);
	// the schema must survive the full re-marshal.
	ad := config.Defaults()
	m2 := roundtrip(t, &ad, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"write a detailed multi-step analysis of distributed systems tradeoffs"}],`+rf+`}`)
	if _, ok := m2["thinking"]; !ok {
		t.Fatalf("expected the complex prompt to force a re-encode (thinking injected): %v", m2)
	}
	if _, ok := m2["response_format"]; !ok {
		t.Fatalf("response_format dropped when the router re-encoded the request: %v", m2)
	}
}

func TestRouterBadUpstreamReturns502(t *testing.T) {
	cfg := config.Defaults()
	rt, err := Start(&cfg, "http://127.0.0.1:1", 0, "") // nothing listening -> dial fails
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// A genuine upstream failure (not a client cancellation) must surface as 502,
	// via the custom ErrorHandler -- which logs nothing to the shared terminal.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("bad upstream: want 502, got %d", resp.StatusCode)
	}
}

func TestRouterDisablesThinkingForTrivial(t *testing.T) {
	cfg := config.Defaults()
	m := roundtrip(t, &cfg, "/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`)
	kw, ok := m["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("expected chat_template_kwargs for trivial prompt, got %v", m)
	}
	if kw["enable_thinking"] != false {
		t.Fatalf("expected enable_thinking=false, got %v", kw["enable_thinking"])
	}
}

type teamBackend struct {
	name string
	got  []byte
	srv  *httptest.Server
}

func newTeamBackend(name string) *teamBackend {
	b := &teamBackend{name: name}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"who":"` + name + `"}`))
	}))
	return b
}

// TestTeamRoutesByModel is the core team-mode guarantee: each request reaches the
// backend its `model` field names, unknown models fall back to main, and the haiku
// route forces thinking off while sonnet/main keep adaptive reasoning.
func TestTeamRoutesByModel(t *testing.T) {
	cfg := config.Defaults()
	main := newTeamBackend("main")
	sonnet := newTeamBackend("sonnet")
	haiku := newTeamBackend("haiku")
	defer main.srv.Close()
	defer sonnet.srv.Close()
	defer haiku.srv.Close()

	routes := []Route{
		{Model: "small-4b", Upstream: sonnet.srv.URL, Think: ""},
		{Model: "tiny-0.8b", Upstream: haiku.srv.URL, Think: "low"},
	}
	rt, err := StartTeam(&cfg, routes, nil, "", main.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(model string) string {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		who, _ := m["who"].(string)
		return who
	}

	if who := post("big-35b"); who != "main" {
		t.Errorf("unknown model should fall back to main, got %q", who)
	}
	if who := post("small-4b"); who != "sonnet" {
		t.Errorf("sonnet tier mis-routed, got %q", who)
	}
	if who := post("tiny-0.8b"); who != "haiku" {
		t.Errorf("haiku tier mis-routed, got %q", who)
	}

	// The haiku (research) route injects a small thinking budget -- small models need a
	// little thinking to call tools reliably, not none.
	var hk map[string]any
	_ = json.Unmarshal(haiku.got, &hk)
	think, ok := hk["thinking"].(map[string]any)
	if !ok || think["budget_tokens"] == nil {
		t.Errorf("haiku worker should get a low thinking budget, got %v", hk)
	}
}

// TestInjectThinkingPolicyOff: "off" disables thinking even for a prompt the adaptive
// logic would otherwise give a budget, and never adds a thinking budget.
func TestInjectThinkingPolicyOff(t *testing.T) {
	cfg := config.Defaults()
	out := injectThinkingPolicy(&cfg,
		[]byte(`{"model":"x","messages":[{"role":"user","content":"write a detailed analysis of rabbit warren logistics"}]}`), "off")
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	kw, ok := m["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != false {
		t.Fatalf("off must set enable_thinking=false, got %v", m)
	}
	if _, hasThink := m["thinking"]; hasThink {
		t.Fatalf("off must not add a thinking budget, got %v", m)
	}
}

// TestInjectThinkingPolicyLow: "low" injects a small thinking budget even for a trivial
// prompt -- small models need a little thinking to orchestrate tools reliably.
func TestInjectThinkingPolicyLow(t *testing.T) {
	cfg := config.Defaults()
	out := injectThinkingPolicy(&cfg, []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`), "low")
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	think, ok := m["thinking"].(map[string]any)
	if !ok || think["type"] != "enabled" {
		t.Fatalf("low must enable thinking, got %v", m)
	}
	if b, _ := think["budget_tokens"].(float64); b <= 0 || b > 2048 {
		t.Fatalf("low budget should be small and positive, got %v", think["budget_tokens"])
	}
}

// TestInjectThinkingPolicyNonAdaptivePassthrough: in non-adaptive modes the main/sonnet
// tiers are governed by server flags, so the team router must NOT rewrite their requests
// (otherwise it would fight the backend's --reasoning flag).
func TestInjectThinkingPolicyNonAdaptivePassthrough(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reasoning.Mode = "off"
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"write a big complex thing with code"}]}`)
	if out := injectThinkingPolicy(&cfg, body, ""); string(out) != string(body) {
		t.Fatalf("non-adaptive non-worker request must pass through unchanged; got %s", out)
	}
}

// TestTeamLadderEscalation: a subagent (ladderTag) starts on the smallest rung and
// escalates to bigger rungs as its estimated load grows.
func TestTeamLadderEscalation(t *testing.T) {
	cfg := config.Defaults()
	small := newTeamBackend("small")
	mid := newTeamBackend("mid")
	big := newTeamBackend("big")
	defer small.srv.Close()
	defer mid.srv.Close()
	defer big.srv.Close()

	ladder := []Tier{
		{Upstream: small.srv.URL, Think: "low", MaxEstTokens: 4096},
		{Upstream: mid.srv.URL, Think: "low", MaxEstTokens: 16384},
		{Upstream: big.srv.URL, Think: "", MaxEstTokens: 1 << 30},
	}
	rt, err := StartTeam(&cfg, nil, ladder, "worker", small.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(filler int) string {
		body := `{"model":"worker","messages":[{"role":"user","content":"` + strings.Repeat("x", filler) + `"}]}`
		resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		who, _ := m["who"].(string)
		return who
	}

	if who := post(100); who != "small" { // ~tiny -> 0.8B rung
		t.Errorf("tiny request should stay on small, got %q", who)
	}
	if who := post(40000); who != "mid" { // ~10k tokens -> 4B rung
		t.Errorf("medium request should escalate to mid, got %q", who)
	}
	if who := post(120000); who != "big" { // ~30k tokens -> main rung
		t.Errorf("large request should escalate to big, got %q", who)
	}
}

func toolNames(b []byte) []string {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	arr, _ := m["tools"].([]any)
	var out []string
	for _, t := range arr {
		if o, ok := t.(map[string]any); ok {
			if n, ok := o["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

func TestCompactRequestStripsAndMinifies(t *testing.T) {
	body := []byte(`{"model":"m","tools":[` +
		`{"name":"WebSearch","description":"s"},` +
		`{"name":"Bash","description":"b"},` +
		`{"name":"Read","description":"r","input_examples":["x"]},` +
		`{"name":"Edit","description":"e"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	out, before, after := compactRequest(body, []string{"WebSearch", "Read", "Grep", "Glob"})
	if before != 4 || after != 2 {
		t.Fatalf("tool counts before=%d after=%d, want 4->2", before, after)
	}
	got := toolNames(out)
	if len(got) != 2 || got[0] != "WebSearch" || got[1] != "Read" {
		t.Fatalf("expected [WebSearch Read] in order, got %v", got)
	}
	if strings.Contains(string(out), "input_examples") {
		t.Error("input_examples should be minified out")
	}
}

func TestCompactRequestHeadAndAllKeepEverything(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"Bash"},{"name":"Edit"}],"messages":[]}`)
	for _, allow := range [][]string{nil, {"all"}} {
		if _, before, after := compactRequest(body, allow); before != 2 || after != 2 {
			t.Errorf("allow=%v must keep all tools, got %d->%d", allow, before, after)
		}
	}
}

func TestCompactRequestEmptyIntersectionKeepsAll(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"Bash"},{"name":"Edit"}],"messages":[]}`)
	if _, before, after := compactRequest(body, []string{"WebSearch", "Read"}); before != 2 || after != 2 {
		t.Errorf("empty intersection must keep all tools (never zero), got %d->%d", before, after)
	}
}

// TestTeamRouterStripsWorkerToolsOnly: a worker (ladder) request is stripped to its tier's
// allowlist; the HEAD (fallback) request keeps every tool.
func TestTeamRouterStripsWorkerToolsOnly(t *testing.T) {
	cfg := config.Defaults()
	worker := newTeamBackend("worker")
	main := newTeamBackend("main")
	defer worker.srv.Close()
	defer main.srv.Close()

	ladder := []Tier{{Upstream: worker.srv.URL, Think: "low", MaxEstTokens: 1 << 30, Tools: []string{"WebSearch", "Read"}}}
	rt, err := StartTeam(&cfg, nil, ladder, "worker", main.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	tools := `"tools":[{"name":"WebSearch"},{"name":"Bash"},{"name":"Read"},{"name":"Edit"}]`
	post := func(model string) {
		body := `{"model":"` + model + `",` + tools + `,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("worker") // -> worker backend, stripped to 2
	post("big")    // -> main (fallback), untouched

	if got := toolNames(worker.got); len(got) != 2 {
		t.Errorf("worker tools should be stripped to 2 (WebSearch, Read), got %v", got)
	}
	if got := toolNames(main.got); len(got) != 4 {
		t.Errorf("HEAD (fallback) tools must be untouched (4), got %v", got)
	}
}

// errUpstream serves a fixed status + JSON body, to exercise ModifyResponse.
func errUpstream(t *testing.T, status int, body string) (*Router, func()) {
	t.Helper()
	cfg := config.Defaults()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	rt, err := Start(&cfg, up.URL, 0, "")
	if err != nil {
		up.Close()
		t.Fatal(err)
	}
	return rt, func() { rt.Stop(); up.Close() }
}

func postErr(t *testing.T, rt *Router) (int, string) {
	t.Helper()
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ccPromptTooLong mirrors Claude Code's exact context-overflow detector regex
// (function QX8 in the CLI). If our rewrite matches this, Claude Code treats the
// error as "transcript too long" (compact + retry / manual approval) instead of
// "model unavailable" (fail-closed block in auto mode).
var ccPromptTooLong = regexp.MustCompile(`(?i)prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)

func TestRewriteContextOverflowError(t *testing.T) {
	llamaErr := `{"error":{"code":400,"message":"request (7810 tokens) exceeds the available context size (2048 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":7810,"n_ctx":2048}}`
	rt, done := errUpstream(t, http.StatusBadRequest, llamaErr)
	defer done()

	code, body := postErr(t, rt)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", code)
	}
	var m struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("rewritten body not JSON: %v (%s)", err, body)
	}
	if m.Type != "error" || m.Error.Type != "invalid_request_error" {
		t.Fatalf("want anthropic error envelope, got %s", body)
	}
	mm := ccPromptTooLong.FindStringSubmatch(m.Error.Message)
	if mm == nil {
		t.Fatalf("rewritten message must match Claude Code's detector; got %q", m.Error.Message)
	}
	if mm[1] != "7810" || mm[2] != "2048" {
		t.Fatalf("want 7810 tokens > 2048, got %q", m.Error.Message)
	}
}

// Older/variant engines may omit the structured token fields; the message-regex
// fallback must still recover the counts.
func TestRewriteContextOverflowFromMessageOnly(t *testing.T) {
	llamaErr := `{"error":{"message":"request (5000 tokens) exceeds the available context size (4096 tokens)","type":"exceed_context_size_error"}}`
	rt, done := errUpstream(t, http.StatusBadRequest, llamaErr)
	defer done()
	_, body := postErr(t, rt)
	mm := ccPromptTooLong.FindStringSubmatch(body)
	if mm == nil || mm[1] != "5000" || mm[2] != "4096" {
		t.Fatalf("fallback parse failed; got %s", body)
	}
}

// In a non-adaptive mode the router still runs (for the overflow rewrite + minify) but
// must NOT inject a thinking budget -- on/off/fixed are owned by the server's own flags.
func TestNonAdaptiveModeNoThinkingInjected(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reasoning.Mode = "off"
	m := roundtrip(t, &cfg, "/v1/messages",
		`{"messages":[{"role":"user","content":"write a bunny calculator with full tests"}],"max_tokens":50}`)
	if _, ok := m["thinking"]; ok {
		t.Fatalf("non-adaptive mode must not inject thinking, got %v", m)
	}
	if _, ok := m["chat_template_kwargs"]; ok {
		t.Fatalf("non-adaptive mode must not touch chat_template_kwargs, got %v", m)
	}
}

func TestRewritePassesThroughNonOverflow(t *testing.T) {
	other := `{"error":{"code":400,"message":"some other problem","type":"invalid_request_error"}}`
	rt, done := errUpstream(t, http.StatusBadRequest, other)
	defer done()
	_, body := postErr(t, rt)
	if strings.Contains(body, "prompt is too long") {
		t.Fatalf("non-overflow 400 must pass through unchanged, got %s", body)
	}
	if !strings.Contains(body, "some other problem") {
		t.Fatalf("original error must be preserved, got %s", body)
	}
}

func maxTokOf(b []byte) (float64, bool) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return 0, false
	}
	v, ok := m["max_tokens"].(float64)
	return v, ok
}

func TestClampMaxTokens(t *testing.T) {
	clamp := func(body string, cap int) ([]byte, bool) {
		t.Helper()
		return clampMaxTokens([]byte(body), cap)
	}
	out, lowered := clamp(`{"max_tokens":8000}`, 1536)
	if v, _ := maxTokOf(out); v != 1536 || !lowered {
		t.Errorf("over-large should clamp to 1536 (lowered=true), got %v lowered=%v", v, lowered)
	}
	out, lowered = clamp(`{"max_tokens":512}`, 1536)
	if v, _ := maxTokOf(out); v != 512 || lowered {
		t.Errorf("already-small should be untouched (512, lowered=false), got %v lowered=%v", v, lowered)
	}
	out, lowered = clamp(`{"messages":[]}`, 1536)
	if v, _ := maxTokOf(out); v != 1536 || !lowered {
		t.Errorf("absent should be set to cap (1536, lowered=true), got %v lowered=%v", v, lowered)
	}
	out, lowered = clamp(`{"max_tokens":8000}`, 0)
	if v, _ := maxTokOf(out); v != 8000 || lowered {
		t.Errorf("cap<=0 must be a no-op (8000, lowered=false), got %v lowered=%v", v, lowered)
	}
}

// The worker tier's generation must be capped (loop guard) while the HEAD model's is not.
func TestTeamRouterCapsWorkerMaxTokens(t *testing.T) {
	cfg := config.Defaults()
	worker := newTeamBackend("worker")
	main := newTeamBackend("main")
	defer worker.srv.Close()
	defer main.srv.Close()

	ladder := []Tier{{Upstream: worker.srv.URL, Think: "low", MaxEstTokens: 1 << 30, Tools: []string{"all"}, MaxTokens: 1536}}
	rt, err := StartTeam(&cfg, nil, ladder, "worker", main.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(model string) {
		body := `{"model":"` + model + `","max_tokens":8000,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("worker") // -> worker, capped
	post("big")    // -> main (fallback), uncapped

	if v, _ := maxTokOf(worker.got); v != 1536 {
		t.Errorf("worker max_tokens must be capped to 1536, got %v", v)
	}
	if v, _ := maxTokOf(main.got); v != 8000 {
		t.Errorf("HEAD max_tokens must be untouched (8000), got %v", v)
	}

	// Session counters: one worker request was capped, both requests counted by name.
	st := rt.Stats()
	if st.CapsLowered != 1 {
		t.Errorf("caps-lowered counter = %d, want 1", st.CapsLowered)
	}
	if st.Requests["main"] != 1 {
		t.Errorf("main request count = %d, want 1", st.Requests["main"])
	}
}

// TestRouterCountsOverflows: each context-overflow rewrite increments the session
// counter surfaced at shutdown ("how often did the agent hit the context wall?").
func TestRouterCountsOverflows(t *testing.T) {
	llamaErr := `{"error":{"code":400,"message":"request (7810 tokens) exceeds the available context size (2048 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":7810,"n_ctx":2048}}`
	rt, done := errUpstream(t, http.StatusBadRequest, llamaErr)
	defer done()
	postErr(t, rt)
	postErr(t, rt)
	if n := rt.Stats().Overflows; n != 2 {
		t.Fatalf("overflow counter = %d, want 2", n)
	}
}

// TestMarkDeadReroutes is the watchdog contract: after MarkDead, ladder picks skip the
// dead rung (escalating to the next alive one, or main when none are left) and the
// dead-skip counter + dead list record it for the end-of-session summary.
func TestMarkDeadReroutes(t *testing.T) {
	cfg := config.Defaults()
	haiku := newTeamBackend("haiku")
	sonnet := newTeamBackend("sonnet")
	main := newTeamBackend("main")
	defer haiku.srv.Close()
	defer sonnet.srv.Close()
	defer main.srv.Close()

	ladder := []Tier{
		{Name: "haiku", Upstream: haiku.srv.URL, Think: "low", MaxEstTokens: 4096},
		{Name: "sonnet", Upstream: sonnet.srv.URL, Think: "", MaxEstTokens: 1 << 30},
	}
	rt, err := StartTeam(&cfg, nil, ladder, "worker", main.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func() string {
		body := `{"model":"worker","messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		who, _ := m["who"].(string)
		return who
	}

	if who := post(); who != "haiku" { // healthy: tiny request -> first rung
		t.Fatalf("healthy ladder should pick haiku, got %q", who)
	}
	rt.MarkDead("haiku", haiku.srv.URL)
	if who := post(); who != "sonnet" { // haiku dead -> next alive rung
		t.Errorf("dead haiku rung should escalate to sonnet, got %q", who)
	}
	rt.MarkDead("sonnet", sonnet.srv.URL)
	if who := post(); who != "main" { // whole ladder dead -> fallback
		t.Errorf("fully dead ladder should fall back to main, got %q", who)
	}

	st := rt.Stats()
	if st.DeadSkips != 2 {
		t.Errorf("dead-skip counter = %d, want 2", st.DeadSkips)
	}
	if len(st.Dead) != 2 || st.Dead[0] != "haiku" || st.Dead[1] != "sonnet" {
		t.Errorf("dead list = %v, want [haiku sonnet]", st.Dead)
	}
	if st.Requests["haiku"] != 1 || st.Requests["sonnet"] != 1 || st.Requests["main"] != 1 {
		t.Errorf("per-rung request counts = %v, want haiku/sonnet/main = 1 each", st.Requests)
	}
}

// MarkDead must be idempotent (the watchdog can only fire once per worker, but a
// pinned route + ladder rung can share an upstream).
func TestMarkDeadIdempotent(t *testing.T) {
	r := &Router{}
	r.MarkDead("haiku", "http://127.0.0.1:9999")
	r.MarkDead("haiku", "http://127.0.0.1:9999")
	st := r.Stats()
	if len(st.Dead) != 1 {
		t.Fatalf("dead list = %v, want exactly [haiku]", st.Dead)
	}
}

func TestStatsString(t *testing.T) {
	s := Stats{
		Requests:    map[string]int{"sonnet": 3, "haiku": 12, "main": 5},
		Dead:        []string{"sonnet"},
		Overflows:   2,
		CapsLowered: 4,
		DeadSkips:   1,
	}
	got := s.String()
	want := "requests: haiku=12 main=5 sonnet=3  overflows-rewritten=2  caps-lowered=4  dead-skips=1  dead: sonnet"
	if got != want {
		t.Errorf("Stats.String() =\n  %q\nwant\n  %q", got, want)
	}
	if empty := (Stats{}).String(); empty != "requests: none" {
		t.Errorf("empty stats = %q, want \"requests: none\"", empty)
	}
	if s := (Stats{Requests: map[string]int{}, InfoPinned: 3}).String(); !strings.Contains(s, "info-pinned=3") {
		t.Errorf("stats should report info pins: %q", s)
	}
}

// A compaction request that no longer fits the window must be trimmed (oldest
// messages dropped) so the summary itself has room to complete -- otherwise the
// session enters the overflow -> truncated-summary -> overflow death loop.
func TestTrimCompaction(t *testing.T) {
	const window = 8192 // tokens; budget = window - summaryRoomTokens = 2048 tokens
	chunk := strings.Repeat("old conversation content. ", 200)
	instruction := `{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. Wrap your summary in <summary></summary>."}`
	var sb strings.Builder
	sb.WriteString(`{"model":"main","messages":[`)
	for i := 0; i < 10; i++ {
		sb.WriteString(`{"role":"user","content":"` + chunk + `"},`)
		sb.WriteString(`{"role":"assistant","content":"` + chunk + `"},`)
		sb.WriteString(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t` + string(rune('0'+i)) + `","content":"` + chunk + `"}]},`)
	}
	sb.WriteString(`{"role":"user","content":"recent question"},`)
	sb.WriteString(instruction)
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	out := trimCompaction(body, window)
	if len(out) >= len(body) {
		t.Fatalf("oversized compaction should be trimmed: %d -> %d bytes", len(body), len(out))
	}
	if got := reasoning.EstimateInputTokens(out); got > window-summaryRoomTokens+64 {
		t.Errorf("trimmed compaction still too big: %d tokens (budget %d)", got, window-summaryRoomTokens)
	}
	var m struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil || len(m.Messages) < 2 {
		t.Fatalf("trimmed body unparseable: %v", err)
	}
	last := m.Messages[len(m.Messages)-1]
	if !strings.Contains(string(last.Content), "detailed summary") {
		t.Errorf("the summarize instruction must survive the trim, got %s", last.Content)
	}
	first := m.Messages[0]
	if first.Role != "user" || strings.Contains(string(first.Content), "tool_result") {
		t.Errorf("trimmed transcript must open on a plain user message, got role=%s %s", first.Role, first.Content)
	}

	// A non-compaction request is never trimmed, no matter how big.
	huge := []byte(`{"model":"main","messages":[{"role":"user","content":"` + strings.Repeat("x", 60000) + `"}]}`)
	if got := trimCompaction(huge, window); len(got) != len(huge) {
		t.Error("non-compaction request must pass through untouched")
	}
	// A compaction that fits is untouched, and window 0 disables trimming.
	small := []byte(`{"model":"main","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},` + instruction + `]}`)
	if got := trimCompaction(small, window); len(got) != len(small) {
		t.Error("fitting compaction must pass through untouched")
	}
	if got := trimCompaction(body, 0); len(got) != len(body) {
		t.Error("window 0 must disable trimming")
	}
}

// The messages a compaction trim drops are archived to .claude-local/
// trimmed-context.md BEFORE they vanish -- they're exactly what the summary
// won't cover, and a failed summary otherwise erases the session's only record.
func TestTrimCompactionArchives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WINC_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude-local"), 0o755); err != nil {
		t.Fatal(err)
	}
	const window = 8192
	chunk := strings.Repeat("ancient history the summary will not cover. ", 200)
	instruction := `{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. Wrap your summary in <summary></summary>."}`
	var sb strings.Builder
	sb.WriteString(`{"model":"main","messages":[`)
	for i := 0; i < 10; i++ {
		sb.WriteString(`{"role":"user","content":"` + chunk + `"},`)
		sb.WriteString(`{"role":"assistant","content":"` + chunk + `"},`)
	}
	sb.WriteString(`{"role":"user","content":"recent question"},`)
	sb.WriteString(instruction)
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	if out := trimCompaction(body, window); len(out) >= len(body) {
		t.Fatal("oversized compaction should have been trimmed")
	}
	archive := filepath.Join(home, ".claude-local", "trimmed-context.md")
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if !strings.Contains(string(data), "ancient history") {
		t.Error("archive must hold the dropped text")
	}
	if !strings.Contains(string(data), "## trimmed ") {
		t.Error("archive entries carry a timestamp header")
	}
	// A fitting compaction trims nothing and writes nothing.
	before, _ := os.Stat(archive)
	small := []byte(`{"model":"main","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},` + instruction + `]}`)
	_ = trimCompaction(small, window)
	if after, _ := os.Stat(archive); after.Size() != before.Size() {
		t.Error("fitting compaction must not write to the archive")
	}
}

// The preflight backstop: a head-bound request that leaves the slot no
// generation room is answered with the compaction signal up front. The server
// would ACCEPT such a request and then stop generating at the wall with no
// error -- the agent receives silently mangled tool calls (observed live for
// 20+ consecutive turns).
func TestRouterBlocksWallRequests(t *testing.T) {
	cfg := config.Defaults()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	rt, err := Start(&cfg, up.URL, 8192, "") // tiny real window
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}` // ~10k est tokens
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader([]byte(big)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wall request: want 400, got %d (%s)", resp.StatusCode, b)
	}
	if ccPromptTooLong.FindStringSubmatch(string(b)) == nil {
		t.Fatalf("the rejection must carry Claude Code's compaction signal, got %s", b)
	}
	if n := rt.Stats().Overflows; n != 1 {
		t.Errorf("preflight block counts as an overflow event, got %d", n)
	}
	// A small request passes through untouched.
	resp2, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("small request must pass, got %d", resp2.StatusCode)
	}
}

// An information-only subagent (read/search/fetch tools -- or none at all) must never
// land on the Head rung, no matter how large its context grows: a second full session
// on the big GPU model is strictly slower than a worker for read-and-report work.
func TestInfoRequestsNeverReachHead(t *testing.T) {
	ladder := []Tier{
		{Name: "haiku", Upstream: "http://h", MaxEstTokens: 2048},
		{Name: "sonnet", Upstream: "http://s", MaxEstTokens: 16384},
		{Name: "escalated", Upstream: "http://main", MaxEstTokens: 1 << 30, Head: true},
	}
	big := strings.Repeat("lots of source file content to read and summarize ", 4000)
	body := func(tools string) []byte {
		return []byte(`{"model":"haiku","messages":[{"role":"user","content":"` + big + `"}],"tools":[` + tools + `]}`)
	}
	rd := `{"name":"Read"},{"name":"Grep"},{"name":"Glob"},{"name":"WebFetch"},{"name":"WebSearch"}`
	if tier, pinned := pickTier(ladder, body(rd)); tier.Name != "sonnet" || !pinned {
		t.Errorf("huge info request should pin to the top worker, got %s (pinned=%v)", tier.Name, pinned)
	}
	// A request that can act (Edit) keeps its right to escalate to the head.
	if tier, pinned := pickTier(ladder, body(rd+`,{"name":"Edit"}`)); tier.Name != "escalated" || pinned {
		t.Errorf("acting request should escalate to head, got %s (pinned=%v)", tier.Name, pinned)
	}
	// Unknown tools (MCP, Bash, ...) also keep the right to escalate.
	if tier, _ := pickTier(ladder, body(`{"name":"Bash"}`)); tier.Name != "escalated" {
		t.Errorf("unknown-tool request should escalate, got %s", tier.Name)
	}
	// No tools at all = pure read-context-and-report -> stays on a worker.
	noTools := []byte(`{"model":"haiku","messages":[{"role":"user","content":"` + big + `"}]}`)
	if tier, pinned := pickTier(ladder, noTools); tier.Name != "sonnet" || !pinned {
		t.Errorf("no-tool request should stay on a worker, got %s (pinned=%v)", tier.Name, pinned)
	}
	// A small info request still starts on the smallest rung -- and doesn't count as
	// pinned, since the cap didn't change the pick.
	small := []byte(`{"model":"haiku","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read"}]}`)
	if tier, pinned := pickTier(ladder, small); tier.Name != "haiku" || pinned {
		t.Errorf("small info request should start small un-pinned, got %s (pinned=%v)", tier.Name, pinned)
	}
	// A ladder without a Head rung (escalation locked) behaves exactly as before.
	workersOnly := ladder[:2]
	if tier, pinned := pickTier(workersOnly, body(rd)); tier.Name != "sonnet" || pinned {
		t.Errorf("head-less ladder should be unchanged, got %s (pinned=%v)", tier.Name, pinned)
	}
}

// A response cut at max_tokens mid-TEXT is continued in place: the client
// receives ONE complete message ending end_turn -- no more half answers the
// agent treats as final. The continuation is the router's own prefill request
// to the same backend, and every token still reaches the client (the
// never-inject rule is about hiding content; nothing is hidden here).
func TestRouterContinuesTruncatedStream(t *testing.T) {
	cfg := config.Defaults()
	var contReqs int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		body, _ := io.ReadAll(rq.Body)
		if strings.Contains(string(body), "\"role\":\"assistant\"") {
			contReqs++
			if !strings.Contains(string(body), "Half an answer") {
				t.Errorf("continuation must carry the partial text as assistant prefill: %s", body)
			}
			if !strings.Contains(string(body), "\"stream\":false") {
				t.Errorf("continuation legs are non-streaming: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":" and the rest."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":4}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Half an answer\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		} {
			_, _ = io.WriteString(w, ev+"\n\n")
		}
	}))
	defer up.Close()
	rt, err := Start(&cfg, up.URL, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"model":"m","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	s := string(raw)
	if !strings.Contains(s, "Half an answer") || !strings.Contains(s, " and the rest.") {
		t.Fatalf("client must receive the full spliced text:\n%s", s)
	}
	if !strings.Contains(s, "\"stop_reason\":\"end_turn\"") {
		t.Errorf("final stop must be the continuation's end_turn:\n%s", s)
	}
	if strings.Contains(s, "\"stop_reason\":\"max_tokens\"") {
		t.Errorf("the max_tokens cut must not leak to the client:\n%s", s)
	}
	if got := strings.Count(s, "\"type\":\"message_stop\""); got != 1 {
		t.Errorf("exactly one message_stop must reach the client, got %d:\n%s", got, s)
	}
	if !strings.Contains(s, "\"output_tokens\":11") {
		t.Errorf("usage must sum both legs (7+4):\n%s", s)
	}
	if contReqs != 1 {
		t.Errorf("expected exactly one continuation leg, got %d", contReqs)
	}
	if n := rt.Stats().Continued; n != 1 {
		t.Errorf("continued counter = %d, want 1", n)
	}
}

// A cut ending in a tool_use block has no prefill form -- it must pass through
// untouched (no continuation request, max_tokens preserved for the client).
func TestRouterNoContinueOnToolUseCut(t *testing.T) {
	cfg := config.Defaults()
	var contReqs int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		body, _ := io.ReadAll(rq.Body)
		if strings.Contains(string(body), "\"role\":\"assistant\"") {
			contReqs++
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Write\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		} {
			_, _ = io.WriteString(w, ev+"\n\n")
		}
	}))
	defer up.Close()
	rt, err := Start(&cfg, up.URL, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"model":"m","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "\"stop_reason\":\"max_tokens\"") {
		t.Errorf("tool_use cut must pass through with its real stop reason:\n%s", raw)
	}
	if contReqs != 0 {
		t.Errorf("tool_use cut must not trigger continuation, got %d legs", contReqs)
	}
	if n := rt.Stats().Continued; n != 0 {
		t.Errorf("continued counter = %d, want 0", n)
	}
}

// The non-streaming shape gets the same treatment: text spliced, usage summed,
// final stop reason replacing the cut.
func TestRouterContinuesTruncatedJSON(t *testing.T) {
	cfg := config.Defaults()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		body, _ := io.ReadAll(rq.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "\"role\":\"assistant\"") {
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":" finished."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":3}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"Started but"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":5}}`))
	}))
	defer up.Close()
	rt, err := Start(&cfg, up.URL, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		bytes.NewReader([]byte(`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	s := string(raw)
	if !strings.Contains(s, "Started but finished.") {
		t.Fatalf("non-stream splice failed:\n%s", s)
	}
	if !strings.Contains(s, "\"stop_reason\":\"end_turn\"") || strings.Contains(s, "max_tokens") {
		t.Errorf("final stop must replace the cut:\n%s", s)
	}
	if n := rt.Stats().Continued; n != 1 {
		t.Errorf("continued counter = %d, want 1", n)
	}
}
