package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFFNPlacementCodec(t *testing.T) {
	if got := plFFN(12); got != "ffn:12" {
		t.Fatalf("plFFN(12) = %q", got)
	}
	for pl, want := range map[string]int{
		"ffn:12": 12, "ffn:1": 1, "ffn:0": 0, "ffn:-3": 0, "ffn:": 0,
		"gpu": 0, "nomtp": 0, "spill": 0, "ffn:x": 0,
	} {
		if got := ffnSpillOf(pl); got != want {
			t.Errorf("ffnSpillOf(%q) = %d, want %d", pl, got, want)
		}
	}
}

// An FFN-spill launch memo replays with its placement intact -- without it the
// replay would pin the FULL model resident and the gate would reject the very
// window the memo promises.
func TestLaunchMemoFFNPlacement(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "Dense-4B-Q4_K_M.gguf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(50 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	const fp = "cafe0123"
	saveLaunchMemo(p, 49152, "q8_0/q4_0", 38.5, fp, plFFN(13))
	ctx, ct, tps, pl := loadLaunchMemo(p, fp)
	if ctx != 49152 || ct != "q8_0/q4_0" || tps != 38.5 || pl != "ffn:13" {
		t.Fatalf("ffn memo round-trip failed: %d %q %v %q", ctx, ct, tps, pl)
	}
	if ffnSpillOf(pl) != 13 {
		t.Fatalf("placement should decode 13 spilled blocks, got %d", ffnSpillOf(pl))
	}
}
