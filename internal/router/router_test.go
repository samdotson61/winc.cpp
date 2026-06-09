package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		{Model: "small-4b", Upstream: sonnet.srv.URL, ThinkOff: false},
		{Model: "tiny-0.8b", Upstream: haiku.srv.URL, ThinkOff: true},
	}
	rt, err := StartTeam(&cfg, routes, main.srv.URL)
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

	// The haiku route forces thinking off (fast research fan-out).
	var hk map[string]any
	_ = json.Unmarshal(haiku.got, &hk)
	kw, ok := hk["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != false {
		t.Errorf("haiku worker should get enable_thinking=false, got %v", hk)
	}
}

// TestInjectThinkingPolicyForceOff: forceOff disables thinking even for a prompt the
// adaptive logic would otherwise give a budget, and never adds a thinking budget.
func TestInjectThinkingPolicyForceOff(t *testing.T) {
	cfg := config.Defaults()
	out := injectThinkingPolicy(&cfg,
		[]byte(`{"model":"x","messages":[{"role":"user","content":"write a detailed analysis of rabbit warren logistics"}]}`), true)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	kw, ok := m["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != false {
		t.Fatalf("forceOff must set enable_thinking=false, got %v", m)
	}
	if _, hasThink := m["thinking"]; hasThink {
		t.Fatalf("forceOff must not add a thinking budget, got %v", m)
	}
}

// TestInjectThinkingPolicyNonAdaptivePassthrough: in non-adaptive modes the main/sonnet
// tiers are governed by server flags, so the team router must NOT rewrite their requests
// (otherwise it would fight the backend's --reasoning flag).
func TestInjectThinkingPolicyNonAdaptivePassthrough(t *testing.T) {
	cfg := config.Defaults()
	cfg.Reasoning.Mode = "off"
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"write a big complex thing with code"}]}`)
	if out := injectThinkingPolicy(&cfg, body, false); string(out) != string(body) {
		t.Fatalf("non-adaptive non-worker request must pass through unchanged; got %s", out)
	}
}
