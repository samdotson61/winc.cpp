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

func TestDecideCompaction(t *testing.T) {
	cfg := config.Defaults()

	// A big compaction request: the summarize instruction is the final user message.
	hist := strings.Repeat("lots of prior conversation. ", 4000) // pushes it past the top tier
	body := `{"messages":[` +
		`{"role":"user","content":"` + hist + `"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. Wrap your summary in <summary></summary>."}` +
		`]}`
	d := Decide(&cfg, []byte(body))
	if d.EnableThinking || d.BudgetTokens != 0 || !d.Set {
		t.Fatalf("compaction request must run think-free, got %+v", d)
	}

	// Claude Code appends an ASSISTANT PREFILL after the summarize instruction, so the
	// instruction is the last USER message but NOT messages[n-1]. Detection must scan
	// back past the prefill -- otherwise (the bug) the summary request keeps an adaptive
	// thinking budget and a deep reasoner exhausts it inside <think>, returning an empty
	// summary ("summarization produced empty response").
	prefill := `{"messages":[` +
		`{"role":"user","content":"` + hist + `"},` +
		`{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. Your summary should include the following sections."},` +
		`{"role":"assistant","content":"<summary>"}` +
		`]}`
	if d := Decide(&cfg, []byte(prefill)); d.EnableThinking || d.BudgetTokens != 0 {
		t.Fatalf("compaction with a trailing assistant prefill must run think-free, got %+v", d)
	}

	// The compaction SUMMARY's section headers persist in history -> a normal later
	// turn must NOT be flagged as compaction (it should think normally).
	later := `{"messages":[` +
		`{"role":"user","content":"Primary Request and Intent: build winc. Key Technical Concepts: Go, llama.cpp. Pending Tasks: none."},` +
		`{"role":"user","content":"now implement the export feature"}` +
		`]}`
	d = Decide(&cfg, []byte(later))
	if !d.EnableThinking {
		t.Fatalf("post-compaction normal turn must still think, got %+v", d)
	}
}
