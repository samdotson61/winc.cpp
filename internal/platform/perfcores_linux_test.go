package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func writePolicy(t *testing.T, dir, name, freq, cpus string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "cpuinfo_max_freq"), []byte(freq+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "related_cpus"), []byte(cpus+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPerfCoresFromSysfs(t *testing.T) {
	// big.LITTLE 4P+4E: everything above the slow class counts.
	d := t.TempDir()
	writePolicy(t, d, "policy0", "1800000", "0 1 2 3")
	writePolicy(t, d, "policy4", "3200000", "4 5 6 7")
	if got := perfCoresFromSysfs(d); got != 4 {
		t.Fatalf("4P+4E = %d, want 4", got)
	}

	// prime/gold/silver 1+4+3: prime AND gold beat the silvers -> 5, not 1.
	d = t.TempDir()
	writePolicy(t, d, "policy0", "2000000", "0 1 2")
	writePolicy(t, d, "policy3", "2800000", "3 4 5 6")
	writePolicy(t, d, "policy7", "3300000", "7")
	if got := perfCoresFromSysfs(d); got != 5 {
		t.Fatalf("1+4+3 = %d, want 5", got)
	}

	// uniform cores: no split to exploit -> 0 (engine default).
	d = t.TempDir()
	writePolicy(t, d, "policy0", "3000000", "0 1 2 3 4 5 6 7")
	if got := perfCoresFromSysfs(d); got != 0 {
		t.Fatalf("uniform = %d, want 0", got)
	}

	// missing/empty dir -> 0.
	if got := perfCoresFromSysfs(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("missing dir = %d, want 0", got)
	}
}
