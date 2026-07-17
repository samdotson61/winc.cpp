package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// buildLargeBody synthesizes a chat request approximating a deep agent turn:
// ~n message turns of a few hundred tokens each, plus a tools array.
func buildLargeBody(nMsgs int) []byte {
	var msgs []map[string]any
	para := strings.Repeat("The quick brown fox jumps over the lazy dog near the riverbank. ", 40)
	for i := 0; i < nMsgs; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": fmt.Sprintf("[turn %d] %s", i, para)})
	}
	tools := []map[string]any{}
	for i := 0; i < 12; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("tool_%d", i), "description": para[:200],
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}})
	}
	b, _ := json.Marshal(map[string]any{"model": "claude", "max_tokens": 4096, "messages": msgs, "tools": tools})
	return b
}

// BenchmarkRouterProcess measures winc's added per-request CPU: the parse +
// minify + encode pipeline the router runs on every chat POST body.
func BenchmarkRouterProcess(b *testing.B) {
	for _, n := range []int{50, 400} {
		body := buildLargeBody(n)
		b.Run(fmt.Sprintf("msgs=%d/bytes=%d", n, len(body)), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				if p := parseReq(body); p != nil {
					p.compact(nil)
					_ = p.encode()
				}
			}
		})
	}
}
