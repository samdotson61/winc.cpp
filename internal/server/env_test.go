package server

import "testing"

func TestEnvWithLibPathLinuxPrepends(t *testing.T) {
	env := envWithLibPath("linux", "/opt/winc/bin", []string{"FOO=1", "LD_LIBRARY_PATH=/usr/lib"})
	var got string
	foo := false
	for _, e := range env {
		if len(e) >= 16 && e[:16] == "LD_LIBRARY_PATH=" {
			got = e
		}
		if e == "FOO=1" {
			foo = true
		}
	}
	if got != "LD_LIBRARY_PATH=/opt/winc/bin:/usr/lib" {
		t.Errorf("got %q, want prepended bin dir", got)
	}
	if !foo {
		t.Error("unrelated env var was dropped")
	}
}

func TestEnvWithLibPathDarwinFresh(t *testing.T) {
	env := envWithLibPath("darwin", "/x/bin", []string{"A=b"})
	found := false
	for _, e := range env {
		if e == "DYLD_LIBRARY_PATH=/x/bin" {
			found = true
		}
	}
	if !found {
		t.Errorf("DYLD_LIBRARY_PATH not set: %v", env)
	}
}

func TestEnvWithLibPathWindowsNoop(t *testing.T) {
	in := []string{"A=b"}
	env := envWithLibPath("windows", "/x/bin", in)
	if len(env) != 1 || env[0] != "A=b" {
		t.Errorf("windows should be a no-op, got %v", env)
	}
}
