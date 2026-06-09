package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"winc/internal/config"
)

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
	rt, err := Start(cfg, up.URL)
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

func TestRouterBadUpstreamReturns502(t *testing.T) {
	cfg := config.Defaults()
	rt, err := Start(&cfg, "http://127.0.0.1:1") // nothing listening -> dial fails
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
	rt, err := StartTeam(&cfg, routes, nil, "", main.srv.URL)
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
	rt, err := StartTeam(&cfg, nil, ladder, "worker", small.srv.URL)
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
	rt, err := StartTeam(&cfg, nil, ladder, "worker", main.srv.URL)
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
