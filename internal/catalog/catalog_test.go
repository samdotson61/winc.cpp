package catalog

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/paths"
)

// Quality floor: nothing below IQ3-class quantization anywhere, and nothing below
// Q4_K_M for models under 14B -- destructive quants make small models useless
// (coding especially, where syntax precision matters). Enforced here so future
// catalog additions can't erode the floor.
func TestCatalogQuantFloor(t *testing.T) {
	params := regexp.MustCompile(`(\d+(?:\.\d+)?)B`)
	quant := regexp.MustCompile(`(?i)(UD-)?(IQ\d|TQ\d|Q\d)[A-Z0-9_]*`)
	bannedAnywhere := regexp.MustCompile(`(?i)^(UD-)?(IQ[12]|Q[12]|TQ)`)
	okUnder14B := regexp.MustCompile(`(?i)^(UD-)?(Q4_K_M|Q4_K_XL|Q[568])`)
	c := Load(nil)
	for _, m := range c.Models {
		q := quant.FindString(m.File)
		if q == "" {
			t.Errorf("%s: no quant tag found in file %q", m.Alias, m.File)
			continue
		}
		if bannedAnywhere.MatchString(q) {
			t.Errorf("%s: quant %s is below the IQ3 floor (>10%% degradation)", m.Alias, q)
		}
		pm := params.FindStringSubmatch(m.Name)
		if pm == nil {
			t.Errorf("%s: no parameter count found in name %q", m.Alias, m.Name)
			continue
		}
		b, _ := strconv.ParseFloat(pm[1], 64)
		if b < 14 && !okUnder14B.MatchString(q) {
			t.Errorf("%s (%sB): quant %s is below Q4_K_M -- too destructive under 14B", m.Alias, pm[1], q)
		}
	}
}

func TestParseCatalog(t *testing.T) {
	if _, ok := parseCatalog([]byte("not json")); ok {
		t.Error("garbage should not parse")
	}
	if _, ok := parseCatalog([]byte(`{"models":[]}`)); ok {
		t.Error("too-small catalog should be rejected")
	}
	if _, ok := parseCatalog(catalogJSON); !ok {
		t.Error("embedded catalog should parse")
	}
}

func TestOnDiskCatalogOverride(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	// No override yet -> embedded loads.
	if Load(nil).Find("qwen3.5-9b") == nil {
		t.Fatal("embedded catalog should load when no override present")
	}
	override := `{"tiers":{"nano":"x"},"models":[
	  {"tier":"nano","alias":"sentinel-model","name":"S","size":"1 GB","repo":"u/r","file":"s.gguf"},
	  {"tier":"nano","alias":"m2","name":"2","size":"1 GB","repo":"u/r","file":"2.gguf"},
	  {"tier":"nano","alias":"m3","name":"3","size":"1 GB","repo":"u/r","file":"3.gguf"},
	  {"tier":"nano","alias":"m4","name":"4","size":"1 GB","repo":"u/r","file":"4.gguf"},
	  {"tier":"nano","alias":"m5","name":"5","size":"1 GB","repo":"u/r","file":"5.gguf"}]}`
	if err := os.WriteFile(paths.CatalogPath(), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Load(nil)
	if c.Find("sentinel-model") == nil {
		t.Error("on-disk override should be used")
	}
	if c.Find("qwen3.5-9b") != nil {
		t.Error("override should fully replace the embedded base")
	}
	// Corrupt override -> fall back to embedded (never break ls).
	if err := os.WriteFile(paths.CatalogPath(), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Load(nil).Find("qwen3.5-9b") == nil {
		t.Error("corrupt override should fall back to embedded")
	}
}

func TestCatalogLoads(t *testing.T) {
	c := Load(nil)
	if len(c.Models) < 15 {
		t.Fatalf("too few models: %d", len(c.Models))
	}
	if c.Find("qwen3.5-9b") == nil {
		t.Fatal("alias qwen3.5-9b not found")
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
	// Dense small coders -> the shared tiny Qwen3.5 draft (same tokenizer family).
	for _, target := range []string{"qwen3.5-9b", "omnicoder-9b"} {
		if d := c.DraftFor(c.Find(target)); d == nil || d.Alias != "qwen3.5-0.8b" {
			t.Errorf("%s draft = %v, want qwen3.5-0.8b", target, d)
		}
	}
	// MoE -> no draft (speculative decoding is net-negative on MoE; MTP handles it).
	if d := c.DraftFor(c.Find("qwen3.6-35b")); d != nil {
		t.Errorf("MoE qwen3.6-35b should have no draft, got %q", d.Alias)
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
	// Standard Qwen models (MoE and dense) point at their MTP variant.
	for std, want := range map[string]string{
		"qwen3.6-35b":    "qwen3.6-35b-mtp",
		"qwen3.6-35b-q4": "qwen3.6-35b-q4-mtp",
		"qwen3.6-35b-q5": "qwen3.6-35b-q5-mtp",
		"qwen3.6-27b":    "qwen3.6-27b-mtp",
		"qwen3.6-27b-q5": "qwen3.6-27b-q5-mtp",
		"qwen3.6-27b-q6": "qwen3.6-27b-q6-mtp",
		"qwen3.5-9b":     "qwen3.5-9b-mtp",
	} {
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
	if m := c.Find("gemma4-12b"); m == nil || m.LocalFile() != m.File {
		t.Errorf("standard model LocalFile should equal File")
	}
}

func mtpOf(m *Model) string {
	if m == nil {
		return "(nil)"
	}
	return m.Mtp
}

// Gemma 4 ships MTP heads as a separate file in the same repo; every Gemma 4
// entry must reference one so the download flow can offer it.
func TestGemmaMTPHeads(t *testing.T) {
	c := Load(nil)
	n := 0
	for _, m := range c.Models {
		if !strings.Contains(m.Repo, "gemma-4") {
			continue
		}
		n++
		if m.MtpHead == "" {
			t.Errorf("%s: gemma-4 entry without mtp_head", m.Alias)
		} else if !strings.HasSuffix(m.MtpHead, "-MTP.gguf") {
			t.Errorf("%s: mtp_head %q should end in -MTP.gguf", m.Alias, m.MtpHead)
		}
	}
	if n < 6 {
		t.Errorf("expected at least 6 gemma-4 entries, found %d", n)
	}
}

func TestCustomModelMerge(t *testing.T) {
	c := Load([]config.CustomModel{{Alias: "my-test-model", Repo: "u/r", File: "f.gguf", Tier: "nano"}})
	if c.Find("my-test-model") == nil {
		t.Fatal("custom model not merged")
	}
}
