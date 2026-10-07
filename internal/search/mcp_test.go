package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeBackend answers from a canned result set or error.
type fakeBackend struct {
	rs  []Result
	err error
	got string
	n   int
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Search(_ context.Context, q string, n int) ([]Result, error) {
	f.got, f.n = q, n
	return f.rs, f.err
}

func rpc(t *testing.T, backend Backend, lines ...string) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, l := range lines {
		in.WriteString(l + "\n")
	}
	var out bytes.Buffer
	if err := Serve(&in, &out, backend, 0); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("non-JSON response line %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// The MCP handshake: initialize answers with capabilities + server info, the
// initialized notification gets NO response, tools/list advertises web_search.
func TestMCPHandshakeAndList(t *testing.T) {
	resps := rpc(t, &fakeBackend{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.224"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses (notification is silent), got %d: %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(map[string]any)["name"] != "winc" {
		t.Errorf("bad initialize result: %v", init)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("tools capability missing: %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "web_search" {
		t.Errorf("tools/list: %v", tools)
	}
	schema := tools[0].(map[string]any)["inputSchema"].(map[string]any)
	if schema["required"].([]any)[0] != "query" {
		t.Errorf("query must be required: %v", schema)
	}
}

// tools/call round trip: the query and cap reach the backend, the text result
// is the numbered list, isError is false.
func TestMCPToolsCall(t *testing.T) {
	fb := &fakeBackend{rs: []Result{{Title: "A", URL: "https://a", Snippet: "sa"}, {Title: "B", URL: "https://b"}}}
	resps := rpc(t, fb, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"llama.cpp server","max_results":2}}}`)
	if len(resps) != 1 {
		t.Fatalf("got %d responses", len(resps))
	}
	if fb.got != "llama.cpp server" || fb.n != 2 {
		t.Errorf("backend got %q n=%d", fb.got, fb.n)
	}
	res := resps[0]["result"].(map[string]any)
	if res["isError"] != false {
		t.Errorf("isError should be false: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, w := range []string{"1. A", "https://a", "sa", "2. B"} {
		if !strings.Contains(text, w) {
			t.Errorf("result text missing %q:\n%s", w, text)
		}
	}
	if resps[0]["id"] != float64(7) {
		t.Errorf("id not echoed: %v", resps[0]["id"])
	}
}

// Failures are tool errors that NAME the cause; the model never sees an empty list.
func TestMCPToolsCallErrors(t *testing.T) {
	for name, c := range map[string]struct {
		backend Backend
		call    string
		want    string
	}{
		"rate limited": {&fakeBackend{err: errors.New("duckduckgo: rate limited (HTTP 429)")},
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"x"}}}`, "429"},
		"challenge": {&fakeBackend{err: ErrBlocked},
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"x"}}}`, "CAPTCHA"},
		"search off": {nil,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"x"}}}`, "turned off"},
		"empty query": {&fakeBackend{},
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"  "}}}`, "required"},
	} {
		resps := rpc(t, c.backend, c.call)
		res, ok := resps[0]["result"].(map[string]any)
		if !ok || res["isError"] != true {
			t.Errorf("%s: want isError tool result, got %v", name, resps[0])
			continue
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, c.want) {
			t.Errorf("%s: error text %q does not name the cause %q", name, text, c.want)
		}
	}
	// Protocol-level mistakes are JSON-RPC errors.
	resps := rpc(t, &fakeBackend{},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`not json at all`,
	)
	if len(resps) != 3 {
		t.Fatalf("got %d responses: %v", len(resps), resps)
	}
	for i, code := range []float64{-32602, -32601, -32700} {
		e, ok := resps[i]["error"].(map[string]any)
		if !ok || e["code"] != code {
			t.Errorf("response %d: want error code %v, got %v", i, code, resps[i])
		}
	}
}

// A genuinely empty answer is reported as such (and is not an error).
func TestMCPEmptyAnswer(t *testing.T) {
	resps := rpc(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"zzz"}}}`)
	res := resps[0]["result"].(map[string]any)
	if res["isError"] != false || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "No results") {
		t.Errorf("empty answer: %v", res)
	}
}
