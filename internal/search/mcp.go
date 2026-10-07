package search

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Serve speaks MCP over a stdio-style pair: newline-delimited JSON-RPC 2.0
// requests on r, responses on w (the transport Claude Code uses for a
// `"type": "stdio"` server). Methods: initialize, notifications/initialized,
// ping, tools/list, tools/call. One tool, web_search. Stdlib only.
//
// A search failure is reported as a tool result with isError=true whose text
// names the cause (rate limit, challenge page, HTTP status, timeout) -- the
// model sees WHY, instead of the silent "0 results" of the built-in tool.
// defaultN is the configured default result count; backend may be nil when
// search is off (every call then errors with ErrOff).
func Serve(r io.Reader, w io.Writer, backend Backend, defaultN int) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		if resp, ok := handle(context.Background(), &req, backend, defaultN); ok {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolSchema is the single tool advertised by tools/list.
var toolSchema = map[string]any{
	"name": "web_search",
	"description": "Search the web (runs locally through winc; Claude Code's built-in WebSearch does not work on a local model). " +
		"Returns a numbered list of title, URL and snippet. Follow up with WebFetch on a URL to read a page.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "The search query."},
			"max_results": map[string]any{"type": "integer", "description": "Results to return (default 5, max 10).", "minimum": 1, "maximum": HardMaxResults},
		},
		"required": []string{"query"},
	},
}

// handle dispatches one request. ok=false means "send nothing" (a notification).
func handle(ctx context.Context, req *rpcRequest, backend Backend, defaultN int) (rpcResponse, bool) {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = "2025-06-18"
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "winc", "version": "1"},
			"instructions":    "web_search runs locally through winc. Use it for anything that needs current information; the built-in WebSearch returns nothing on a local model.",
		}
		return resp, true
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return resp, false
	case "ping":
		resp.Result = map[string]any{}
		return resp, !isNotification
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{toolSchema}}
		return resp, true
	case "tools/call":
		var p struct {
			Name string `json:"name"`
			Args struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp, true
		}
		if p.Name != "web_search" {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", p.Name)}
			return resp, true
		}
		resp.Result = callWebSearch(ctx, backend, defaultN, p.Args.Query, p.Args.MaxResults)
		return resp, true
	}
	if isNotification {
		return resp, false
	}
	resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	return resp, true
}

// callWebSearch runs the query and shapes the MCP tool result. Errors are
// tool-level (isError) so the model reads the cause as text and can react --
// a JSON-RPC error would surface only as an opaque failure.
func callWebSearch(ctx context.Context, backend Backend, defaultN int, query string, n int) map[string]any {
	query = strings.TrimSpace(query)
	if query == "" {
		return toolError("web_search: 'query' is required")
	}
	if backend == nil {
		backend = Failing(ErrOff)
	}
	n = ClampN(n, defaultN)
	ctx, cancel := context.WithTimeout(ctx, Timeout+time.Second)
	defer cancel()
	start := time.Now()
	rs, err := backend.Search(ctx, query, n)
	if err != nil {
		logLine("provider=%s query=%q n=%d ERROR %v (%dms)", backend.Name(), query, n, err, time.Since(start).Milliseconds())
		return toolError(fmt.Sprintf("web_search failed (%s backend): %v", backend.Name(), err))
	}
	logLine("provider=%s query=%q n=%d results=%d (%dms)", backend.Name(), query, n, len(rs), time.Since(start).Milliseconds())
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": Format(query, rs)}},
		"isError": false,
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

// LogPath, when set, receives one line per tools/call (query, provider, result
// count or the error, latency) -- the audit trail that shows a search really
// ran through winc, and what came back. Best-effort; never fails a call.
var LogPath string

func logLine(format string, a ...any) {
	if LogPath == "" {
		return
	}
	f, err := os.OpenFile(LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
