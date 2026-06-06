// Package server launches and supervises the llama-server / llama-swap processes
// and waits for readiness. Cross-platform (uses os/exec only).
package server

import (
	"os"
	"os/exec"
	"sync"
)

// Proc is a supervised child process logging to a file.
type Proc struct {
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
	mu   sync.Mutex
	dead bool
}

// Start launches bin with args, redirecting output to logPath, detached from our
// stdio so it doesn't corrupt the agent's TUI.
func Start(bin string, args []string, logPath string) (*Proc, error) {
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	c := exec.Command(bin, args...)
	c.Stdout = lf
	c.Stderr = lf
	if err := c.Start(); err != nil {
		lf.Close()
		return nil, err
	}
	p := &Proc{cmd: c, log: lf, done: make(chan struct{})}
	go func() {
		c.Wait()
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// Dead reports whether the process has exited.
func (p *Proc) Dead() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// Pid returns the OS process id.
func (p *Proc) Pid() int {
	if p != nil && p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// Stop kills the process and releases the log file. Idempotent.
func (p *Proc) Stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if !p.Dead() {
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	if p.log != nil {
		p.log.Close()
		p.log = nil
	}
}
