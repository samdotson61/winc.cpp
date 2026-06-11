// Package reasoning computes the per-request thinking budget for adaptive mode:
// a ceiling scaled to request size, bumped up when the request looks complex.
package reasoning

import (
	"bytes"
	"encoding/json"
	"strings"

	"winc/internal/config"
)

// Decision tells the router how to set thinking on a request.
type Decision struct {
	BudgetTokens   int
	EnableThinking bool // false => inject chat_template_kwargs.enable_thinking=false
	Set            bool // whether to modify the request at all
}

var buildVerbs = []string{
	"write ", "build ", "implement ", "create ", "refactor ", "fix ", "debug ",
	"design ", "optimize ", "add ", "generate ", "rewrite ", "solve ", "explain ",
}

// EstimateInputTokens approximates tokens from the raw request body (~4 chars/token).
func EstimateInputTokens(body []byte) int { return len(body) / 4 }

// IsCompaction reports whether a chat request looks like Claude Code's
// context-compaction (summarize-the-conversation) request.
func IsCompaction(body []byte) bool { return isCompaction(body) }

// Heavy reports whether a request looks compute-heavy for model-tier escalation: it
// carries several fenced code blocks (>=3), i.e. real code/analysis work a tiny model
// handles poorly. A high threshold avoids false-positives from a stray example in the
// system prompt or a tool description. Raw request load is the primary escalation signal;
// this is the orthogonal "kind of task" hint.
func Heavy(body []byte) bool {
	return bytes.Count(body, fence) >= 6 // 6 fences = 3 code blocks (open + close)
}

var (
	fence      = []byte("```")
	toolResult = []byte("tool_result")
	toolUse    = []byte("tool_use")
)

// ContentText flattens an Anthropic content field (a plain string or an array of
// {type,text} blocks) into one string. Exported for the router's compaction-trim
// archive, which flattens the messages it is about to drop.
func ContentText(raw json.RawMessage) string { return contentText(raw) }

// contentText flattens an Anthropic content field (a plain string or an array of
// {type,text} blocks) into one string.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			b.WriteString(bl.Text)
			b.WriteByte(' ')
		}
		return b.String()
	}
	return ""
}

// isCompaction reports whether this looks like Claude Code's context-compaction
// request: the instruction to summarize the whole conversation. It checks only the
// FINAL user message + system prompt (where the instruction lives), not the history
// -- the resulting summary keeps the section HEADERS in context, so matching those
// would wrongly flag every later turn. Summaries need no reasoning, and even a genuine
// "summarize our conversation" ask is fine to run think-free, so this is safe.
func isCompaction(body []byte) bool {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	var last json.RawMessage
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		last = req.Messages[n-1].Content
	}
	return CompactionProbe(req.System, last)
}

// CompactionProbe is the compaction check over already-extracted fields: the
// system prompt plus the FINAL message's content when that message is a user
// message (nil otherwise). Lets the router reuse its single parse of the body
// instead of re-decoding the whole transcript here.
func CompactionProbe(system, lastUserContent json.RawMessage) bool {
	probe := contentText(system)
	if len(lastUserContent) > 0 {
		probe += " " + contentText(lastUserContent)
	}
	s := strings.ToLower(probe)
	return strings.Contains(s, "summary of the conversation") ||
		strings.Contains(s, "detailed summary of") ||
		strings.Contains(s, "wrap your summary")
}

// LooksComplex reports whether a request carries code blocks, tool activity, or a
// build-intent verb. The JSON markers are lowercase literals on the wire, so the
// common case (any agent transcript) is decided by raw byte scans with no
// allocation; only a marker-free body pays for the one lowercased copy the verb
// search needs (verbs appear in user text with arbitrary case).
func LooksComplex(body []byte) bool {
	if bytes.Contains(body, fence) || bytes.Contains(body, toolResult) || bytes.Contains(body, toolUse) {
		return true
	}
	s := strings.ToLower(string(body))
	if strings.Contains(s, "tool_result") || strings.Contains(s, "tool_use") {
		return true
	}
	for _, v := range buildVerbs {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// Decide returns the thinking decision for adaptive mode given a request body.
// Tiers are ascending by max_input_tokens with ascending budgets; the first tier
// whose threshold covers the estimate wins. complexity_boost nudges one tier up
// (more thinking) so short-but-complex prompts aren't starved.
func Decide(cfg *config.Config, body []byte) Decision {
	return DecideFrom(cfg, EstimateInputTokens(body), isCompaction(body), LooksComplex(body))
}

// DecideFrom is Decide with the request signals precomputed -- the router parses
// each request exactly once and feeds the pieces in, instead of this package
// re-decoding the full body per signal.
func DecideFrom(cfg *config.Config, est int, compaction, complex bool) Decision {
	// Compaction is a mechanical summary -- never burn a thinking budget on it (on a
	// big local context that thinking is minutes of pure overhead before the summary).
	if compaction {
		return Decision{BudgetTokens: 0, EnableThinking: false, Set: true}
	}
	a := cfg.Reasoning.Adaptive
	tiers := a.Tiers

	idx := len(tiers) // sentinel -> ceiling
	for i, t := range tiers {
		if est <= t.MaxInputTokens {
			idx = i
			break
		}
	}
	if a.ComplexityBoost && complex && idx < len(tiers) {
		idx++ // bump toward the larger-budget tier (or ceiling)
	}

	budget := a.CeilingBudgetTokens
	if idx < len(tiers) {
		budget = tiers[idx].BudgetTokens
	}
	if budget <= 0 {
		return Decision{BudgetTokens: 0, EnableThinking: false, Set: true}
	}
	return Decision{BudgetTokens: budget, EnableThinking: true, Set: true}
}
