package engine

import "testing"

func hasArg(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestSmallModelSamplingArgs(t *testing.T) {
	q := SmallModelSamplingArgs("/models/Qwen3.5-0.8B-Q4_K_M.gguf")
	if !hasArg(q, "--top-k") || !hasArg(q, "20") || !hasArg(q, "--presence-penalty") {
		t.Errorf("qwen sampling should set top-k 20 + presence penalty, got %v", q)
	}
	g := SmallModelSamplingArgs("/models/gemma-4-e2b-Q4_K_M.gguf")
	if !hasArg(g, "--top-k") || !hasArg(g, "64") {
		t.Errorf("gemma sampling should set top-k 64, got %v", g)
	}
	if SmallModelSamplingArgs("/models/some-llama-8b.gguf") != nil {
		t.Errorf("unknown family should return nil (keep llama.cpp defaults)")
	}
}
