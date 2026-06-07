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

func TestMoEDefaultsForMidAndLarge(t *testing.T) {
	c := Load(nil)
	// Speed-first: tiers that can fit a strong MoE coder must default to it.
	for tier, want := range map[string]string{"mid": "qwen3.6-35b", "large": "qwen3.6-35b-q4"} {
		ms := c.ByTier(tier)
		got := "(none)"
		if len(ms) > 0 {
			got = ms[0].Alias
		}
		if got != want {
			t.Errorf("tier %q default = %q, want MoE %q", tier, got, want)
		}
	}
}

func TestDraftFor(t *testing.T) {
	c := Load(nil)
	// Dense coder -> its same-tokenizer coder draft.
	if d := c.DraftFor(c.Find("qwen2.5-coder-32b")); d == nil || d.Alias != "qwen2.5-coder-0.5b-draft" {
		t.Errorf("qwen2.5-coder-32b draft = %v, want qwen2.5-coder-0.5b-draft", d)
	}
	// MoE -> no draft (speculative decoding is net-negative on MoE).
	if d := c.DraftFor(c.Find("qwen3.6-35b")); d != nil {
		t.Errorf("MoE qwen3.6-35b should have no draft, got %q", d.Alias)
	}
	// Llama reuses the catalogued 1B as its draft.
	if d := c.DraftFor(c.Find("llama3.1-8b")); d == nil || d.Alias != "llama3.2-1b" {
		t.Errorf("llama3.1-8b draft = %v, want llama3.2-1b", d)
	}
	// Referential integrity: every declared draft must resolve to a real entry.
	for _, m := range c.Models {
		if m.Draft != "" && c.Find(m.Draft) == nil {
			t.Errorf("model %s references missing draft %q", m.Alias, m.Draft)
		}
	}
	if c.DraftFor(nil) != nil {
		t.Error("DraftFor(nil) should be nil")
	}
}

func TestCustomModelMerge(t *testing.T) {
	c := Load([]config.CustomModel{{Alias: "my-test-model", Repo: "u/r", File: "f.gguf", Tier: "nano"}})
	if c.Find("my-test-model") == nil {
		t.Fatal("custom model not merged")
	}
}
