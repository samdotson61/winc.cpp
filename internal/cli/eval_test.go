package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/platform"
)

// The eval profile's knobs are measured choices (see eval.go); the server args
// they produce must hold: template-level reasoning off, no speculative draft,
// no MTP, the 16384-token window, q8 KV.
func TestApplyEvalProfileServerArgs(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.DraftModel = "Qwen3.5-0.8B-Q4_K_M.gguf" // a stale pairing must be cleared
	applyEvalProfile(&cfg)

	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 12288, GPUs: []platform.GPUDevice{{TotalMB: 12288}}}
	s := strings.Join(engine.ServerArgs(&cfg, hw, "Qwen3.5-2B-Q4_K_M.gguf", 8099, "", 0), " ")
	for _, want := range []string{"--reasoning off", "-c 16384", "--cache-type-k q8_0", "--cache-type-v q8_0"} {
		if !strings.Contains(s, want) {
			t.Errorf("eval args missing %q: %s", want, s)
		}
	}
	for _, never := range []string{"--spec-", "--parallel", "--reasoning-budget", "draft"} {
		if strings.Contains(s, never) {
			t.Errorf("eval args must not contain %q: %s", never, s)
		}
	}
	if engine.MTPActive(&cfg, "Whatever-MTP.gguf") {
		t.Error("eval profile must disable MTP")
	}
}

// Tier auto-pick: low end leads with gemma4-e2b (the measured-best sub-3GB eval
// judge), 5GB+ leads with the Qwen 4B anchor; each falls back through its
// tier-ordered preference to whatever is downloaded.
func TestEvalPickModel(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.ModelsDir = dir
	cat := catalog.Load(nil)
	mk := func(alias string) string {
		m := cat.Find(alias)
		if m == nil {
			t.Fatalf("catalog is missing %s", alias)
		}
		p := filepath.Join(dir, m.LocalFile())
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	small := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 4096, GPUs: []platform.GPUDevice{{TotalMB: 4096}}}
	big := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16303, GPUs: []platform.GPUDevice{{TotalMB: 16303}}}
	// A 5 GB-class card is now ABOVE the threshold -> the 4B (the eval anchor
	// fits resident at 3.3 GB); under the old 6 GB cutoff it would have settled
	// for the 2B.
	fiveGB := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 5200, GPUs: []platform.GPUDevice{{TotalMB: 5200}}}

	// Nothing downloaded -> "", with advice printed.
	if p, _ := evalPickModel(&cfg, cat, small); p != "" {
		t.Fatalf("no models downloaded should pick nothing, got %s", p)
	}
	// Only the 4B downloaded -> used at every tier (last-resort fallback).
	p4 := mk("qwen3.5-4b")
	if p, _ := evalPickModel(&cfg, cat, small); p != p4 {
		t.Fatalf("small hw with only 4B downloaded should fall back to it, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, big); p != p4 {
		t.Fatalf("big hw with only 4B should use it, got %s", p)
	}
	// Add the 2B -> low end now prefers the 2B over the 4B (gemma still absent);
	// high end keeps the 4B anchor.
	p2 := mk("qwen3.5-2b")
	if p, _ := evalPickModel(&cfg, cat, small); p != p2 {
		t.Fatalf("small hw without gemma should fall back to the 2B, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, big); p != p4 {
		t.Fatalf("big hw should keep the 4B, got %s", p)
	}
	// Add gemma4-e2b -> it becomes the low-end default (12/12 head-to-head winner);
	// 5 GB+ still takes the Qwen 4B anchor.
	pg := mk("gemma4-e2b")
	if p, _ := evalPickModel(&cfg, cat, small); p != pg {
		t.Fatalf("small hw should now default to gemma4-e2b, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, fiveGB); p != p4 {
		t.Fatalf("5 GB-class hw should take the 4B anchor, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, big); p != p4 {
		t.Fatalf("big hw should take the 4B anchor, got %s", p)
	}
}
