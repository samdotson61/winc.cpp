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
	for _, want := range []string{"--reasoning off", "-c 16384", "--cache-type-k q8_0", "--cache-type-v q8_0", "--temp 0", "--top-k 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("eval args missing %q: %s", want, s)
		}
	}
	// eval scoring must be GREEDY, not the model's agent sampling (Qwen temp 0.7)
	for _, never := range []string{"--spec-", "--parallel", "--reasoning-budget", "draft", "--temp 0.7", "--presence-penalty"} {
		if strings.Contains(s, never) {
			t.Errorf("eval args must not contain %q: %s", never, s)
		}
	}
	if engine.MTPActive(&cfg, "Whatever-MTP.gguf") {
		t.Error("eval profile must disable MTP")
	}
}

// A real eval install has the model's vision projector and DFlash head sitting
// next to it (plain `winc -d` fetches both). The eval profile must load
// NEITHER: scoring is text-only (the projector was ~0.7 GB of memory for
// nothing), and draft speculation is off for evals. The two are coupled --
// dflashActive yields to vision -- so turning vision off without DFlash would
// have switched draft speculation ON. The control run proves the files ARE
// detected, so the eval assertions can't pass vacuously.
func TestEvalProfileLoadsNoProjectorOrDraftHead(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "Qwen3.5-4B-Q4_K_M.gguf")
	for _, f := range []string{model, filepath.Join(dir, "Qwen3.5-4B-mmproj.gguf"), filepath.Join(dir, "Qwen3.5-4B-DFlash.gguf")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16384, GPUs: []platform.GPUDevice{{TotalMB: 16384}}}

	plain := config.Defaults()
	if s := strings.Join(engine.ServerArgs(&plain, hw, model, 8099, "", 0), " "); !strings.Contains(s, "--mmproj") {
		t.Fatalf("control: default config should load the projector found next to the model: %s", s)
	}

	cfg := config.Defaults()
	applyEvalProfile(&cfg)
	s := strings.Join(engine.ServerArgs(&cfg, hw, model, 8099, "", 0), " ")
	for _, never := range []string{"--mmproj", "--spec-", "DFlash", "draft"} {
		if strings.Contains(s, never) {
			t.Errorf("eval args must not contain %q: %s", never, s)
		}
	}
	// Speculation flags are appended at launch by SpecArgs (MTP/DFlash + ngram),
	// not ServerArgs -- check exactly what the launch would add.
	if d := engine.SpecArgs(&cfg, hw, model, "", true); d != nil {
		t.Errorf("eval profile must not speculate (no DFlash, no ngram): %v", d)
	}
	// The trap, proven: vision off with DFlash left on engages draft speculation.
	trap := config.Defaults()
	trap.Performance.Vision = "off"
	if engine.DFlashArgs(&trap, hw, model, "") == nil {
		t.Error("control: vision off alone should engage DFlash (else this test can't see the coupling)")
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

	// Nothing downloaded -> "" (silent; cmdServeEval then offers the download prompt).
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

// evalPrefs flips its order at the VRAM threshold: low-end leads with gemma4-e2b,
// 5 GB+ leads with the Qwen 4B anchor. promptDownloadEvalModel recommends prefs[0].
func TestEvalPrefs(t *testing.T) {
	low := evalPrefs(platform.Hardware{VRAMMB: 4096})
	if len(low) == 0 || low[0] != "gemma4-e2b" {
		t.Fatalf("low-end should lead with gemma4-e2b, got %v", low)
	}
	high := evalPrefs(platform.Hardware{VRAMMB: 8192})
	if len(high) == 0 || high[0] != "qwen3.5-4b" {
		t.Fatalf(">=5GB should lead with qwen3.5-4b, got %v", high)
	}
}

// The low-tier preset is throughput-only and SLOTS-only, tiered by memory
// budget: <4 GB pins one eval slot, 4-8 GB pins two, >=8 GB and unknown
// hardware leave the engine defaults untouched. The window stays 16384 on
// every tier -- a smaller pin was measured to round up through the launch
// ladder (1.23.0-jobdar.3), so the args must never carry -c 8192.
func TestApplyEvalTierServerArgs(t *testing.T) {
	cases := []struct {
		name  string
		hw    platform.Hardware
		want  []string
		never []string
	}{
		{"tiny 2GB card", platform.Hardware{OS: "linux", GPUVendor: "nvidia", VRAMMB: 2048, GPUs: []platform.GPUDevice{{TotalMB: 2048}}},
			[]string{"--parallel 1", "-c 16384"}, []string{"-c 8192"}},
		{"small 6GB card", platform.Hardware{OS: "linux", GPUVendor: "nvidia", VRAMMB: 6144, GPUs: []platform.GPUDevice{{TotalMB: 6144}}},
			[]string{"--parallel 2", "-c 16384"}, []string{"-c 8192"}},
		{"big 12GB card", platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 12288, GPUs: []platform.GPUDevice{{TotalMB: 12288}}},
			[]string{"-c 16384"}, []string{"--parallel"}},
		{"unknown hardware", platform.Hardware{OS: "linux", GPUVendor: "none"},
			[]string{"-c 16384"}, []string{"--parallel"}},
	}
	for _, c := range cases {
		cfg := config.Defaults()
		applyEvalProfile(&cfg)
		applyEvalTier(&cfg, c.hw)
		s := strings.Join(engine.ServerArgs(&cfg, c.hw, "Qwen3.5-2B-Q4_K_M.gguf", 8099, "", 0), " ")
		for _, w := range c.want {
			if !strings.Contains(s, w) {
				t.Errorf("%s: missing %q: %s", c.name, w, s)
			}
		}
		for _, n := range c.never {
			if strings.Contains(s, n) {
				t.Errorf("%s: must not contain %q: %s", c.name, n, s)
			}
		}
	}
}

// On arm64 + cpu backend each eval preference tries its -q40 ARM rung first
// (first-downloaded-wins keeps it opt-in); every other install is unchanged.
// CurrentBackend() reads the install marker -- absent in tests -> "", so the
// non-ARM identity path is what's directly exercised here; the expansion shape
// is asserted via the exported pieces it composes from.
func TestEvalArmCPUPrefs(t *testing.T) {
	base := []string{"qwen3.5-4b", "gemma4-e2b"}
	x86 := evalArmCPUPrefs(platform.Hardware{Arch: "amd64"}, base)
	if strings.Join(x86, ",") != "qwen3.5-4b,gemma4-e2b" {
		t.Errorf("x86 prefs changed: %v", x86)
	}
	// arm64 without a cpu-backend marker is also unchanged (GPU installs).
	arm := evalArmCPUPrefs(platform.Hardware{Arch: "arm64"}, base)
	if strings.Join(arm, ",") != "qwen3.5-4b,gemma4-e2b" {
		t.Errorf("arm64 non-cpu prefs changed: %v", arm)
	}
}
