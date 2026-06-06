package catalog

import (
	"testing"

	"winc/internal/config"
)

func TestCatalogLoads(t *testing.T) {
	c := Load(nil)
	if len(c.Models) < 15 {
		t.Fatalf("too few models: %d", len(c.Models))
	}
	if c.Find("qwen2.5-coder-7b") == nil {
		t.Fatal("alias qwen2.5-coder-7b not found")
	}
	for _, m := range c.Models {
		if m.Alias == "" || m.Repo == "" || m.File == "" || m.Tier == "" {
			t.Fatalf("incomplete catalog entry: %+v", m)
		}
	}
	// every tier referenced by a model must have a label
	for _, m := range c.Models {
		if _, ok := c.Tiers[m.Tier]; !ok {
			t.Errorf("model %s has tier %q with no label", m.Alias, m.Tier)
		}
	}
}

func TestVramTier(t *testing.T) {
	cases := map[int]string{2000: "nano", 7000: "small", 16000: "mid", 24000: "large", 70000: "xl"}
	for mb, want := range cases {
		if got := VramTier(mb); got != want {
			t.Errorf("VramTier(%d) = %s, want %s", mb, got, want)
		}
	}
}

func TestCustomModelMerge(t *testing.T) {
	c := Load([]config.CustomModel{{Alias: "my-test-model", Repo: "u/r", File: "f.gguf", Tier: "nano"}})
	if c.Find("my-test-model") == nil {
		t.Fatal("custom model not merged")
	}
}
