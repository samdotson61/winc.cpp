//go:build windows

package server

import "syscall"

// Deprioritize drops the child process to BELOW_NORMAL priority, so under CPU
// contention the OS scheduler favors normal-priority processes (the main
// model's server) over this one. Best-effort: any failure leaves the child at
// normal priority, which is just the old behavior.

const (
	processSetInformation    = 0x0200
	belowNormalPriorityClass = 0x00004000
)

var procSetPriorityClass = kernel32.NewProc("SetPriorityClass")

func (p *Proc) Deprioritize() {
	pid := p.Pid()
	if pid <= 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(processSetInformation, 0, uintptr(pid))
	if h == 0 {
		return
	}
	_, _, _ = procSetPriorityClass.Call(h, belowNormalPriorityClass)
	_ = syscall.CloseHandle(syscall.Handle(h))
}
