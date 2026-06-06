package reasoning

import (
	"strings"
	"testing"

	"winc/internal/config"
)

func TestDecideTiers(t *testing.T) {
	cfg := config.Defaults()

	// tiny, non-complex -> snappy (budget 0, thinking off)
	d := Decide(&cfg, []byte(`{"messages":[{"role":"user","content":"hi there"}]}`))
	if d.BudgetTokens != 0 || d.EnableThinking {
		t.Fatalf("tiny prompt: want budget 0 / off, got %+v", d)
	}

	// short but complex (build verb) -> boosted to a real budget
	d = Decide(&cfg, []byte(`{"messages":[{"role":"user","content":"write me a calculator"}]}`))
	if !d.EnableThinking || d.BudgetTokens <= 0 {
		t.Fatalf("short-complex prompt: want thinking budget, got %+v", d)
	}

	// very large request -> ceiling
	big := `{"x":"` + strings.Repeat("a", 80000) + `"}`
	d = Decide(&cfg, []byte(big))
	if d.BudgetTokens != cfg.Reasoning.Adaptive.CeilingBudgetTokens {
		t.Fatalf("large request: want ceiling %d, got %d", cfg.Reasoning.Adaptive.CeilingBudgetTokens, d.BudgetTokens)
	}
}
