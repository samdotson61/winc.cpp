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

// Tier auto-pick: >=6GB-class prefers the 4B (judgment), below prefers the 2B
// (speed/fit); a missing preferred model falls back to the downloaded other.
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
	// Only the 4B downloaded -> small hardware still uses it (fallback).
	p4 := mk("qwen3.5-4b")
	if p, _ := evalPickModel(&cfg, cat, small); p != p4 {
		t.Fatalf("small hw with only 4B downloaded should fall back to it, got %s", p)
	}
	// Both downloaded -> small picks 2B, big picks 4B.
	p2 := mk("qwen3.5-2b")
	if p, _ := evalPickModel(&cfg, cat, small); p != p2 {
		t.Fatalf("small hw should prefer the 2B, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, big); p != p4 {
		t.Fatalf("big hw should prefer the 4B, got %s", p)
	}
	if p, _ := evalPickModel(&cfg, cat, fiveGB); p != p4 {
		t.Fatalf("5 GB-class hw should now prefer the 4B (dropped threshold), got %s", p)
	}
}
