package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

// Every sizing-relevant input must move the fingerprint; an unrelated knob
// must not (the remembered stepping survives it).
func TestLaunchFingerprint(t *testing.T) {
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 28000, GPUs: []platform.GPUDevice{{TotalMB: 16000}, {TotalMB: 12000}}}
	cfg := config.Defaults()
	base := launchFingerprint(&cfg, hw)

	mode := cfg
	mode.Performance.Context = "auto" // default is "optimal"
	if launchFingerprint(&mode, hw) == base {
		t.Error("context optimal->auto must change the fingerprint")
	}
	par := cfg
	par.Performance.ExtraServerArgs = []string{"--parallel", "2"}
	if launchFingerprint(&par, hw) == base {
		t.Error("--parallel must change the fingerprint (slot split changes sizing)")
	}
	lessVRAM := hw
	lessVRAM.VRAMMB = 16000
	lessVRAM.GPUs = hw.GPUs[:1]
	if launchFingerprint(&cfg, lessVRAM) == base {
		t.Error("a card vanishing must change the fingerprint")
	}
	unrelated := cfg
	unrelated.General.Port = 9999
	if launchFingerprint(&unrelated, hw) != base {
		t.Error("a non-sizing knob must not invalidate the remembered stepping")
	}
}

// The launch memo lets the second start of a model load ONCE at the measured-good
// window instead of re-walking the ladder (minutes of failed jumbo loads).
func TestLaunchMemoRoundTrip(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	dir := t.TempDir()
	mk := func(name string, mb int64) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(mb << 20); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return p
	}
	const fpA, fpB = "aaaa1111", "bbbb2222"
	p := mk("Model-Q4_K_M.gguf", 50)
	if ctx, _, _, _ := loadLaunchMemo(p, fpA); ctx != 0 {
		t.Fatalf("empty memo should miss, got %d", ctx)
	}
	saveLaunchMemo(p, 131072, "q4_0", 89.6, fpA, plGPU)
	ctx, ct, tps, pl := loadLaunchMemo(p, fpA)
	if ctx != 131072 || ct != "q4_0" || tps != 89.6 || pl != plGPU {
		t.Fatalf("memo round-trip failed: %d %q %v %q", ctx, ct, tps, pl)
	}
	// A different fingerprint (changed sizing inputs) must miss, not replay.
	if ctx, _, _, _ := loadLaunchMemo(p, fpB); ctx != 0 {
		t.Fatalf("changed fingerprint should miss, got %d", ctx)
	}
	// Placement survives the round trip: a spill or no-MTP result must replay
	// the same way or the gate/sizing rejects it every start.
	saveLaunchMemo(p, 98304, "q8_0/q4_0", 22, fpA, plSpill) // same fingerprint replaces, never duplicates
	ctx, ct, tps, pl = loadLaunchMemo(p, fpA)
	if ctx != 98304 || ct != "q8_0/q4_0" || tps != 22 || pl != plSpill {
		t.Fatalf("spill memo replace failed: %d %q %v %q", ctx, ct, tps, pl)
	}
	// Two geometries of the same model coexist (e.g. single vs team --parallel).
	saveLaunchMemo(p, 65536, "q8_0", 40, fpB, plNoMTP)
	if ctx, _, _, _ := loadLaunchMemo(p, fpA); ctx != 98304 {
		t.Fatalf("fpA entry must survive an fpB save, got %d", ctx)
	}
	if ctx, _, _, pl := loadLaunchMemo(p, fpB); ctx != 65536 || pl != plNoMTP {
		t.Fatalf("fpB entry should hit with its placement, got %d %q", ctx, pl)
	}
	// A different file size means a different model -> miss (re-measure).
	other := mk("Model2-Q4_K_M.gguf", 60)
	if ctx, _, _, _ := loadLaunchMemo(other, fpA); ctx != 0 {
		t.Fatalf("different model should miss, got %d", ctx)
	}
	// Entries from older formats (here: the 5-field pre-placement form) can never
	// match again: they miss, and the next save purges them.
	if err := os.WriteFile(launchMemoPath(), []byte(launchMemoKey(other)+" 65536 q8_0 37.5 "+fpA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ctx, _, _, _ := loadLaunchMemo(other, fpA); ctx != 0 {
		t.Fatalf("legacy 5-field memo must miss under the placement format, got %d", ctx)
	}
	saveLaunchMemo(other, 49152, "q8_0", 30, fpA, plGPU)
	data, _ := os.ReadFile(launchMemoPath())
	if strings.Count(string(data), launchMemoKey(other)) != 1 {
		t.Fatalf("legacy line must be purged on save:\n%s", data)
	}
}

// The placement gate's residency arithmetic: a forced-full-GPU load must drop
// free dedicated VRAM by at least half the model's size, and only positive
// evidence (probe data on both sides + a known model size) may reject.
func TestResidencyBroken(t *testing.T) {
	if !residencyBroken(26000, 25000, 19000) {
		t.Error("a 19 GB model that moved free VRAM by 1 GB is not resident (the observed sysmem-fallback shape)")
	}
	if residencyBroken(26000, 4000, 19000) {
		t.Error("a full-size VRAM drop is resident")
	}
	if residencyBroken(26000, 15000, 19000) {
		t.Error("a drop larger than half the model (KV sizing varies) must pass")
	}
	if residencyBroken(0, 4000, 19000) {
		t.Error("no pre-load probe data must never reject")
	}
	if residencyBroken(26000, 0, 19000) {
		t.Error("no post-load probe data must never reject")
	}
	if residencyBroken(26000, 25000, 0) {
		t.Error("unknown model size must never reject")
	}
}

// The memo applies only when winc chose the sizing; explicit settings run as written.
func TestAutoSized(t *testing.T) {
	cfg := config.Defaults()
	if !autoSized(&cfg) {
		t.Error("defaults (auto/auto) should be auto-sized")
	}
	cfg.Performance.Context = "32768"
	if autoSized(&cfg) {
		t.Error("explicit context must disable the launch memo")
	}
	cfg = config.Defaults()
	cfg.Performance.CacheType = "q8_0"
	if autoSized(&cfg) {
		t.Error("explicit cache_type must disable the launch memo")
	}
}
