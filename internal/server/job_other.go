//go:build !windows && !linux

package server

import "os/exec"

// No parent-death mechanism is wired on this OS; children are still stopped by
// winc's own Stop() on a normal exit (pre-1.5 behavior).
func configureChild(c *exec.Cmd) {}

func addToJob(c *exec.Cmd) {}
