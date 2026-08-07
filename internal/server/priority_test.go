package server

import "testing"

// Deprioritize is best-effort: it must be safe to call on procs that never
// started or already died, on every platform.
func TestDeprioritizeSafeOnMissingProcess(t *testing.T) {
	var p *Proc
	p.Deprioritize() // nil receiver: Pid() returns 0, no-op

	empty := &Proc{}
	empty.Deprioritize() // no cmd/process: Pid() returns 0, no-op
}
