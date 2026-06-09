//go:build linux

package server

import (
	"os/exec"
	"syscall"
)

// configureChild asks the kernel to SIGKILL the child if winc dies first, so a
// hard kill of winc can't leave llama-server holding the GPU. Set before Start.
func configureChild(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// addToJob is a no-op on Linux (pdeathsig is configured pre-start).
func addToJob(c *exec.Cmd) {}
