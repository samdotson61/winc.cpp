package engine

import (
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

func hasArg(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestFamilySamplingArgs(t *testing.T) {
	q := FamilySamplingArgs("/models/Qwen3.6-35B-A3B-UD-IQ3_S.gguf")
	if !hasArg(q, "--top-k") || !hasArg(q, "20") || !hasArg(q, "--presence-penalty") {
		t.Errorf("qwen sampling should set top-k 20 + presence penalty, got %v", q)
	}
	g := FamilySamplingArgs("/models/gemma-4-26B-A4B-it.gguf")
	if !hasArg(g, "--temp") || !hasArg(g, "1.0") || !hasArg(g, "64") {
		t.Errorf("gemma sampling should set temp 1.0 / top-k 64, got %v", g)
	}
	if FamilySamplingArgs("/models/some-llama-8b.gguf") != nil {
		t.Errorf("unknown family should return nil (keep llama.cpp defaults)")
	}
}

// TestServerArgsAppliesFamilySampling: family sampling now applies to ANY model via
// ServerArgs -- including a big gemma MAIN, which previously ran at llama.cpp's default temp.
func TestServerArgsAppliesFamilySampling(t *testing.T) {
	cfg := config.Defaults()
	args := ServerArgs(&cfg, platform.Hardware{}, "/models/gemma-4-26B-A4B-it.gguf", 8080, "", 4096)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--temp 1.0") {
		t.Errorf("a gemma main should get Gemma sampling (temp 1.0) via ServerArgs; got: %s", joined)
	}
}
