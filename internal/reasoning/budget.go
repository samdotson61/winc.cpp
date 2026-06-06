// Package reasoning computes the per-request thinking budget for adaptive mode:
// a ceiling scaled to request size, bumped up when the request looks complex.
package reasoning

import (
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

func looksComplex(body []byte) bool {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "```") || strings.Contains(s, "tool_result") || strings.Contains(s, "tool_use") {
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
	a := cfg.Reasoning.Adaptive
	tiers := a.Tiers
	est := EstimateInputTokens(body)

	idx := len(tiers) // sentinel -> ceiling
	for i, t := range tiers {
		if est <= t.MaxInputTokens {
			idx = i
			break
		}
	}
	if a.ComplexityBoost && looksComplex(body) && idx < len(tiers) {
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
