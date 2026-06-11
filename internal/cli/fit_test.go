package cli

import (
	"os"
	"path/filepath"
	"testing"

	"winc/internal/config"
)

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
	p := mk("Model-Q4_K_M.gguf", 50)
	if ctx, _, _ := loadLaunchMemo(p); ctx != 0 {
		t.Fatalf("empty memo should miss, got %d", ctx)
	}
	saveLaunchMemo(p, 131072, "q4_0", 89.6)
	ctx, ct, tps := loadLaunchMemo(p)
	if ctx != 131072 || ct != "q4_0" || tps != 89.6 {
		t.Fatalf("memo round-trip failed: %d %q %v", ctx, ct, tps)
	}
	saveLaunchMemo(p, 98304, "q8_0", 72) // replaces, never appends duplicates
	ctx, ct, tps = loadLaunchMemo(p)
	if ctx != 98304 || ct != "q8_0" || tps != 72 {
		t.Fatalf("memo replace failed: %d %q %v", ctx, ct, tps)
	}
	// A different file size means a different model -> miss (re-measure).
	other := mk("Model2-Q4_K_M.gguf", 60)
	if ctx, _, _ := loadLaunchMemo(other); ctx != 0 {
		t.Fatalf("different model should miss, got %d", ctx)
	}
	// An entry from before the speed field existed (3 fields) still resolves; its
	// missing speed reads as 0 so the caller measures once and rewrites it.
	if err := os.WriteFile(launchMemoPath(), []byte(launchMemoKey(other)+" 65536 q8_0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, ct, tps = loadLaunchMemo(other)
	if ctx != 65536 || ct != "q8_0" || tps != 0 {
		t.Fatalf("legacy 3-field memo should load with tps=0: %d %q %v", ctx, ct, tps)
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
