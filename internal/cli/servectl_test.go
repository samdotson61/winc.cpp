package cli

import (
	"encoding/json"
	"os"
	"testing"

	"winc/internal/platform"
)

// The stamp is the whole safety story: it must be stable for a live process
// and empty for a dead one, so stop can prove a PID is still the process it
// recorded before signalling it.
func TestProcessStampSelf(t *testing.T) {
	a := platform.ProcessStamp(os.Getpid())
	if a == "" {
		t.Fatal("own process must stamp")
	}
	if b := platform.ProcessStamp(os.Getpid()); b != a {
		t.Fatalf("stamp must be stable: %q vs %q", a, b)
	}
	if got := platform.ProcessStamp(1<<30 - 7); got != "" {
		t.Fatalf("absent PID must stamp empty, got %q", got)
	}
}

// A stale pidfile whose PID has died (or been recycled) must be cleaned up
// without anything being signalled, and stop must report nothing running.
func TestStopServeStaleRecord(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	writeServeStateFor(t, 999999999, "winc.exe|1.2") // never a live stamp match
	if _, stopped := stopServe(); stopped {
		t.Fatal("a dead/recycled record must not count as a stop")
	}
	if _, err := os.Stat(serveStatePath()); !os.IsNotExist(err) {
		t.Fatal("stale pidfile must be removed")
	}
}

func writeServeStateFor(t *testing.T, pid int, stamp string) {
	t.Helper()
	st := serveState{PID: pid, Stamp: stamp, Port: 0, Args: []string{"serve"}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(serveStatePath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
