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
