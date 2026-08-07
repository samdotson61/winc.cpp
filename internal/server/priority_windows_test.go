//go:build windows

package server

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

var procGetPriorityClass = kernel32.NewProc("GetPriorityClass")

// Live check: Deprioritize on a real child must actually land BELOW_NORMAL,
// as read back through GetPriorityClass — not just return without error.
func TestDeprioritizeLive(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "child.log")
	cmd := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	p, err := Start(cmd, []string{"/c", "ping", "-n", "3", "127.0.0.1"}, logPath)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer p.Stop()

	p.Deprioritize()

	const processQueryLimitedInformation = 0x1000
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(p.Pid()))
	if h == 0 {
		t.Fatalf("open child process %d", p.Pid())
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	cls, _, _ := procGetPriorityClass.Call(h)
	if cls != belowNormalPriorityClass {
		t.Fatalf("priority class = %#x, want BELOW_NORMAL (%#x)", cls, belowNormalPriorityClass)
	}
}
