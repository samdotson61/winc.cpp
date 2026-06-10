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
	if ctx, _ := loadLaunchMemo(p); ctx != 0 {
		t.Fatalf("empty memo should miss, got %d", ctx)
	}
	saveLaunchMemo(p, 131072, "q4_0")
	ctx, ct := loadLaunchMemo(p)
	if ctx != 131072 || ct != "q4_0" {
		t.Fatalf("memo round-trip failed: %d %q", ctx, ct)
	}
	saveLaunchMemo(p, 98304, "q8_0") // replaces, never appends duplicates
	ctx, ct = loadLaunchMemo(p)
	if ctx != 98304 || ct != "q8_0" {
		t.Fatalf("memo replace failed: %d %q", ctx, ct)
	}
	// A different file size means a different model -> miss (re-measure).
	other := mk("Model2-Q4_K_M.gguf", 60)
	if ctx, _ := loadLaunchMemo(other); ctx != 0 {
		t.Fatalf("different model should miss, got %d", ctx)
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
