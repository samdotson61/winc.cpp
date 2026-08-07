//go:build !windows

package server

import "syscall"

// Deprioritize renices the child process to +10, so under CPU contention the
// scheduler favors nice-0 processes (the main model's server) over this one.
// Best-effort: any failure leaves the child at its inherited niceness, which is
// just the old behavior.
func (p *Proc) Deprioritize() {
	if pid := p.Pid(); pid > 0 {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
	}
}
