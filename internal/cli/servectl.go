package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

// serveState is the pidfile a running `winc serve` leaves in the install dir,
// so `winc stop` / `winc restart` from ANOTHER terminal can find it. Every pid
// carries the ProcessStamp recorded at write time: stop refuses to touch any
// process whose live stamp no longer matches -- a recycled PID (even one
// recycled into a different winc, e.g. an agent session) is never killed.
// Agent launches (`winc -s ...`) deliberately write no pidfile: their server
// belongs to the agent's terminal, and restart-from-outside would wreck the
// session mid-flight.
type serveState struct {
	PID      int       `json:"pid"`
	Stamp    string    `json:"stamp"`
	Port     int       `json:"port"`
	Args     []string  `json:"args"` // the exact serve invocation, for restart
	Children []childID `json:"children,omitempty"`
	Started  time.Time `json:"started"`
}

type childID struct {
	PID   int    `json:"pid"`
	Stamp string `json:"stamp"`
}

func serveStatePath() string { return filepath.Join(paths.InstallDir(), ".winc-serve.json") }

// writeServeState records this process as the running serve. Best-effort: a
// failure only costs stop/restart discoverability, never the serve itself.
func writeServeState(port int, args []string, childPIDs ...int) {
	st := serveState{
		PID:     os.Getpid(),
		Stamp:   platform.ProcessStamp(os.Getpid()),
		Port:    port,
		Args:    args,
		Started: time.Now(),
	}
	for _, p := range childPIDs {
		if p > 0 {
			st.Children = append(st.Children, childID{PID: p, Stamp: platform.ProcessStamp(p)})
		}
	}
	if b, err := json.MarshalIndent(st, "", " "); err == nil {
		_ = os.WriteFile(serveStatePath(), b, 0o644)
	}
}

func clearServeState() { _ = os.Remove(serveStatePath()) }

// readServeState loads the pidfile, or nil when none exists / it is unreadable.
func readServeState() *serveState {
	b, err := os.ReadFile(serveStatePath())
	if err != nil {
		return nil
	}
	var st serveState
	if json.Unmarshal(b, &st) != nil || st.PID <= 0 {
		return nil
	}
	return &st
}

// stopServe stops a recorded serve. Returns (state, true) when a live serve
// was found and stopped, (nil, false) when nothing was running. Only
// stamp-verified processes are ever signalled -- winc kills what it started,
// nothing else, and says so when a stale record doesn't match.
func stopServe() (*serveState, bool) {
	st := readServeState()
	if st == nil {
		return nil, false
	}
	live := platform.ProcessStamp(st.PID)
	if live == "" {
		// The serve is already gone; sweep any recorded children it may have
		// orphaned (macOS has no parent-death reaping), then clean up.
		sweepChildren(st)
		clearServeState()
		return nil, false
	}
	if live != st.Stamp {
		ui.Warn("stale server record: PID %d is now a different process (%s) - not touching it", st.PID, live)
		clearServeState()
		return nil, false
	}
	ui.Info("stopping winc server (PID %d, up since %s)...", st.PID, st.Started.Format("15:04:05"))
	if p, err := os.FindProcess(st.PID); err == nil {
		_ = p.Kill()
	}
	// Windows reaps the children via the job object, Linux via pdeathsig; the
	// explicit sweep covers macOS and any straggler.
	deadline := time.Now().Add(10 * time.Second)
	for platform.ProcessStamp(st.PID) == st.Stamp && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	sweepChildren(st)
	waitPortFree(st.Port, 15*time.Second)
	clearServeState()
	ui.Good("server stopped.")
	return st, true
}

func sweepChildren(st *serveState) {
	for _, c := range st.Children {
		if c.PID <= 0 || c.Stamp == "" {
			continue
		}
		if platform.ProcessStamp(c.PID) != c.Stamp {
			continue // gone, or the PID belongs to someone else now -- hands off
		}
		if p, err := os.FindProcess(c.PID); err == nil {
			_ = p.Kill()
		}
	}
}

// waitPortFree waits until the port accepts a bind (i.e. the old server let go).
func waitPortFree(port int, timeout time.Duration) {
	if port <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// cmdStop stops the running `winc serve` (started in any terminal).
func cmdStop() int {
	if _, stopped := stopServe(); !stopped {
		ui.Info("no winc server is running (nothing recorded, or the recorded one is gone)")
	}
	return 0
}

// cmdRestart stops the running serve (when there is one) and starts a fresh
// one IN THIS terminal: with no argument it replays the exact recorded serve
// invocation (model, --multi, every flag); with a model argument it starts a
// plain `serve <model>` instead. When nothing was running it simply starts.
func cmdRestart(args []string) int {
	old, _ := stopServe()
	serveArgs := []string{}
	switch {
	case len(args) > 0:
		serveArgs = args
	case old != nil && len(old.Args) > 0:
		serveArgs = old.Args
	}
	if len(serveArgs) > 0 {
		ui.Info("starting: winc serve %s", joinArgs(serveArgs))
	} else {
		ui.Info("starting: winc serve (default model)")
	}
	return cmdServe(serveArgs)
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
