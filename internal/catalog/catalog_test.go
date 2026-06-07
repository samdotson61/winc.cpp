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

func TestMTPVariants(t *testing.T) {
	c := Load(nil)
	// Standard Qwen3.6 models point at their MTP variant.
	for std, want := range map[string]string{"qwen3.6-35b": "qwen3.6-35b-mtp", "qwen3.6-27b": "qwen3.6-27b-mtp"} {
		m := c.Find(std)
		if m == nil || m.Mtp != want {
			t.Errorf("%s.Mtp = %q, want %q", std, mtpOf(m), want)
		}
		v := c.Find(want)
		if v == nil {
			t.Fatalf("MTP variant %q missing from catalogue", want)
		}
		// The MTP variant saves under a distinct, MTP-tagged local name (no collision
		// with the standard model, and filename-detectable at launch).
		if v.Save == "" || v.LocalFile() != v.Save {
			t.Errorf("%s should have a distinct save name, got %q", want, v.Save)
		}
		if v.LocalFile() == m.LocalFile() {
			t.Errorf("%s local name collides with standard %s (%s)", want, std, v.LocalFile())
		}
	}
	// Models without a save name fall back to File.
	if m := c.Find("qwen3.6-27b"); m == nil || m.LocalFile() != m.File {
		t.Errorf("standard model LocalFile should equal File")
	}
}

func mtpOf(m *Model) string {
	if m == nil {
		return "(nil)"
	}
	return m.Mtp
}

func TestCustomModelMerge(t *testing.T) {
	c := Load([]config.CustomModel{{Alias: "my-test-model", Repo: "u/r", File: "f.gguf", Tier: "nano"}})
	if c.Find("my-test-model") == nil {
		t.Fatal("custom model not merged")
	}
}
