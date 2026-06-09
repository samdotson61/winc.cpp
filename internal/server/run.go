// Package server launches and supervises the llama-server / llama-swap processes
// and waits for readiness. Cross-platform (uses os/exec only).
package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	c.Env = EnvWithLibPath(filepath.Dir(bin)) // find .so/.dylib shipped beside the binary
	c.Stdout = lf
	c.Stderr = lf
	configureChild(c) // linux: pdeathsig, so children die if winc is hard-killed
	if err := c.Start(); err != nil {
		lf.Close()
		return nil, err
	}
	addToJob(c) // windows: job object w/ KILL_ON_JOB_CLOSE; best-effort
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

// EnvWithLibPath returns the environment with binDir added to the dynamic linker's
// library search path, so shared libraries shipped next to the engine binary load
// at runtime. No-op on Windows (the executable's own directory is searched there).
func EnvWithLibPath(binDir string) []string {
	return envWithLibPath(runtime.GOOS, binDir, os.Environ())
}

func envWithLibPath(goos, binDir string, environ []string) []string {
	var key string
	switch goos {
	case "linux":
		key = "LD_LIBRARY_PATH"
	case "darwin":
		key = "DYLD_LIBRARY_PATH"
	default:
		return environ
	}
	prefix := key + "="
	val := binDir
	out := make([]string, 0, len(environ)+1)
	for _, e := range environ {
		if strings.HasPrefix(e, prefix) {
			if cur := e[len(prefix):]; cur != "" {
				val = binDir + ":" + cur
			}
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+val)
}
