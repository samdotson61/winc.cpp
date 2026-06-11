package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Auto-continuation: when a response ends with stop_reason "max_tokens" and the
// model was mid-TEXT, the router re-prompts the SAME backend with the partial
// answer as an assistant prefill (the /v1/messages contract: a trailing
// assistant message is continued, not answered) and splices the continuation
// into the same client response -- so the agent receives ONE complete message
// instead of a half answer it treats as final. Cloud parity for the places
// local models hit caps the cloud rarely does (worker loop-guard caps, small
// windows).
//
// The never-inject rule holds: the client receives EVERY token of the final
// message, so its transcript and token accounting stay truthful; only the
// serving layer worked harder to produce it. Continuation never applies to
// responses ending in tool_use or thinking (no prefill exists for structured
// blocks), to tiny-cap requests (Claude Code sends max_tokens=1 connectivity
// probes), or to non-Anthropic paths (the OpenAI shape has no prefill
// semantics).
const (
	contMaxLegs       = 2   // extra generations spliced onto a cut response
	contMinReqTokens  = 256 // ignore deliberate tiny caps (probes)
	contLegTimeoutSec = 600 // a continuation leg is a full local generation
)

// contInfo carries what a continuation needs from the request side, attached to
// the proxied request's context by the chat handlers.
type contInfo struct {
	body   []byte // the FINAL upstream request bytes (post-rewrite)
	target string // backend base URL the request was routed to
}

type contKeyType struct{}

var contKey contKeyType

// continuable reports whether the request is eligible at all (path + cap size);
// the response side decides the rest.
func continuable(path string, body []byte) bool {
	if !strings.HasSuffix(path, "/v1/messages") {
		return false
	}
	var m struct {
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	return m.MaxTokens == 0 || m.MaxTokens >= contMinReqTokens
}

// maybeContinue is wired into the proxy's ModifyResponse chain: it wraps
// eligible chat responses so a max_tokens-cut TEXT answer is continued in
// place. Fail-open everywhere: any anomaly forwards the original bytes.
func (r *Router) maybeContinue(resp *http.Response) {
	if resp == nil || resp.Request == nil || resp.StatusCode != http.StatusOK {
		return
	}
	info, _ := resp.Request.Context().Value(contKey).(*contInfo)
	if info == nil {
		return
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "text/event-stream"):
		pr, pw := io.Pipe()
		orig := resp.Body
		resp.Body = pr
		// The spliced stream can be LONGER than the original: any recorded
		// length would make the proxy stop copying mid-synthesis.
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		go r.continueStream(orig, pw, info)
	case strings.Contains(ct, "json"):
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return
		}
		out := r.continueJSON(body, info)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	}
}

// contClient: continuation legs are full local generations; never let the
// default client timeout cut one.
var contClient = &http.Client{Timeout: contLegTimeoutSec * time.Second}

// continuationLeg asks the backend to continue partial text (non-streaming) and
// returns (appendText, stopReason, outputTokens). Empty stop reason = failure;
// the caller forwards what it has.
func continuationLeg(info *contInfo, partial string) (string, string, int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(info.body, &m) != nil {
		return "", "", 0
	}
	var msgs []json.RawMessage
	if json.Unmarshal(m["messages"], &msgs) != nil {
		return "", "", 0
	}
	pre, err := json.Marshal(map[string]any{"role": "assistant", "content": partial})
	if err != nil {
		return "", "", 0
	}
	msgs = append(msgs, pre)
	nb, err := json.Marshal(msgs)
	if err != nil {
		return "", "", 0
	}
	m["messages"] = nb
	sf, _ := json.Marshal(false)
	m["stream"] = sf
	// The continuation is the router's own request: thinking would restart the
	// reasoning phase instead of resuming the text, so it is forced off.
	delete(m, "thinking")
	m["chat_template_kwargs"] = mergeKwargs(m["chat_template_kwargs"], "enable_thinking", false)
	req, err := json.Marshal(m)
	if err != nil {
		return "", "", 0
	}
	resp, err := contClient.Post(info.target+"/v1/messages", "application/json", bytes.NewReader(req))
	if err != nil {
		return "", "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.StopReason == "" {
		return "", "", 0
	}
	text := ""
	for _, c := range out.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return text, out.StopReason, out.Usage.OutputTokens
}

// continueJSON handles the non-streaming shape: splice continuations into the
// final text block and sum the usage.
func (r *Router) continueJSON(body []byte, info *contInfo) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	var stop string
	if json.Unmarshal(m["stop_reason"], &stop) != nil || stop != "max_tokens" {
		return body
	}
	var content []map[string]any
	if json.Unmarshal(m["content"], &content) != nil || len(content) == 0 {
		return body
	}
	last := content[len(content)-1]
	if last["type"] != "text" {
		return body // tool_use / thinking cuts have no prefill -- forward as-is
	}
	partial, _ := last["text"].(string)
	if strings.TrimSpace(partial) == "" {
		return body
	}
	var usage map[string]any
	_ = json.Unmarshal(m["usage"], &usage)
	added := 0
	final := stop
	for leg := 0; leg < contMaxLegs && final == "max_tokens"; leg++ {
		text, st, toks := continuationLeg(info, partial)
		if st == "" {
			break
		}
		partial += text
		added += toks
		final = st
		r.noteContinued()
	}
	if final == stop && added == 0 {
		return body
	}
	last["text"] = partial
	if usage != nil {
		if ot, ok := usage["output_tokens"].(float64); ok {
			usage["output_tokens"] = int(ot) + added
		}
		if ub, err := json.Marshal(usage); err == nil {
			m["usage"] = ub
		}
	}
	if cb, err := json.Marshal(content); err == nil {
		m["content"] = cb
	}
	if sb, err := json.Marshal(final); err == nil {
		m["stop_reason"] = sb
	}
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return body
}

// continueStream transforms the SSE event stream. It forwards everything as it
// arrives, holding back only the final content_block_stop until the
// message_delta reveals the stop reason -- when that says max_tokens and the
// open block is TEXT, the held stop is dropped, continuation text is emitted
// as additional deltas on the SAME block, and the closing events are
// re-synthesized with the final stop reason and summed usage. Any anomaly
// flushes held lines and degrades to plain passthrough.
func (r *Router) continueStream(src io.ReadCloser, dst *io.PipeWriter, info *contInfo) {
	defer src.Close()
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	write := func(s string) bool {
		_, err := io.WriteString(dst, s+"\n")
		return err == nil
	}
	var (
		held     []string // content_block_stop event lines awaiting the verdict
		curType  string   // current content block type
		curIndex = -1
		partial  strings.Builder // accumulated text of the CURRENT text block
		passthru bool            // anomaly -> verbatim forwarding
		drop     bool            // synthesis done -> discard the superseded original tail
	)
	fail := func() { // flush held + passthrough the rest verbatim
		for _, h := range held {
			if !write(h) {
				break
			}
		}
		held = nil
		passthru = true
	}
	for sc.Scan() {
		line := sc.Text()
		if drop {
			continue // our synthesized ending replaced the original tail
		}
		if passthru {
			if !write(line) {
				break
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		data, isData := strings.CutPrefix(trimmed, "data: ")
		if !isData {
			// event:/blank/comment lines: hold the content_block_stop event header
			// until its data line tells us the index; everything else flows.
			if strings.HasPrefix(trimmed, "event: content_block_stop") || strings.HasPrefix(trimmed, "event: message_delta") || strings.HasPrefix(trimmed, "event: message_stop") {
				held = append(held, line)
				continue
			}
			if len(held) > 0 {
				held = append(held, line) // blanks between held events stay held
				continue
			}
			if !write(line) {
				break
			}
			continue
		}
		var probe struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &probe) != nil {
			fail()
			if !write(line) {
				break
			}
			continue
		}
		switch probe.Type {
		case "content_block_start":
			curType, curIndex = probe.Block.Type, probe.Index
			if curType == "text" {
				partial.Reset()
			}
		case "content_block_delta":
			if probe.Delta.Type == "text_delta" {
				partial.WriteString(probe.Delta.Text)
			}
		case "content_block_stop", "message_stop":
			held = append(held, line)
			continue
		case "message_delta":
			if probe.Delta.StopReason != "max_tokens" || curType != "text" || strings.TrimSpace(partial.String()) == "" {
				fail()
				if !write(line) {
					return
				}
				continue
			}
			// A genuine text cut: drop the held block_stop, splice continuations.
			added, final := 0, "max_tokens"
			text := partial.String()
			for leg := 0; leg < contMaxLegs && final == "max_tokens"; leg++ {
				t, st, toks := continuationLeg(info, text)
				if st == "" {
					break
				}
				if t != "" {
					db, err := json.Marshal(map[string]any{
						"type":  "content_block_delta",
						"index": curIndex,
						"delta": map[string]any{"type": "text_delta", "text": t},
					})
					if err != nil {
						break
					}
					if !write("event: content_block_delta") || !write("data: "+string(db)) || !write("") {
						return
					}
				}
				text += t
				added += toks
				final = st
				r.noteContinued()
			}
			if final == "max_tokens" && added == 0 {
				fail() // nothing achieved -- forward the original ending untouched
				if !write(line) {
					return
				}
				continue
			}
			held = nil // the original block_stop/blank lines are superseded
			sb, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": curIndex})
			db, _ := json.Marshal(map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": final, "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": probe.Usage.OutputTokens + added},
			})
			mb, _ := json.Marshal(map[string]any{"type": "message_stop"})
			if !write("event: content_block_stop") || !write("data: "+string(sb)) || !write("") ||
				!write("event: message_delta") || !write("data: "+string(db)) || !write("") ||
				!write("event: message_stop") || !write("data: "+string(mb)) || !write("") {
				return
			}
			drop = true // the original message_stop tail is superseded
			continue
		}
		// Default: flush anything held (it wasn't the decision point), then the line.
		if len(held) > 0 {
			for _, h := range held {
				if !write(h) {
					return
				}
			}
			held = nil
		}
		if !write(line) {
			break
		}
	}
	for _, h := range held {
		if !write(h) {
			break
		}
	}
	_ = dst.CloseWithError(sc.Err())
}

func (r *Router) noteContinued() {
	r.mu.Lock()
	r.continued++
	r.mu.Unlock()
}
