//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AddToPath must land in every shell the user might log into: the POSIX rc
// files AND a fish conf.d drop-in -- fish (the default shell on CachyOS and
// friends) never reads .bashrc/.zshrc/.profile, which made winc "never on
// PATH" there. Idempotent, and RemoveFromPath cleans all of it.
func TestPathAllShells(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin")
	dir := "/opt/winc"

	if OnPath(dir) {
		t.Fatal("fresh home must not report on-PATH")
	}
	if err := AddToPath(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".bashrc"))
	if err != nil || !strings.Contains(string(b), "export PATH=\""+dir) {
		t.Errorf(".bashrc missing the export: %v\n%s", err, b)
	}
	fb, err := os.ReadFile(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish"))
	if err != nil || !strings.Contains(string(fb), dir) {
		t.Errorf("fish conf.d drop-in missing: %v", err)
	}
	// ~/.local/bin gets a symlink (works in shells whose rc files we can't know).
	link := filepath.Join(home, ".local", "bin", "winc")
	if cur, err := os.Readlink(link); err != nil || cur != filepath.Join(dir, "winc") {
		t.Errorf("~/.local/bin/winc symlink wrong: %q err=%v", cur, err)
	}
	// A user's own regular file at that path is never replaced.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AddToPath(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a user-owned file in ~/.local/bin must never be replaced by the symlink")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := AddToPath(dir); err != nil { // restore the symlink for the removal check below
		t.Fatal(err)
	}
	if !OnPath(dir) {
		t.Error("recorded dir must count as on PATH")
	}
	if err := AddToPath(dir); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	if strings.Count(string(b2), pathMarker) != 1 {
		t.Error("AddToPath must be idempotent")
	}
	if err := RemoveFromPath(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish")); err == nil {
		t.Error("fish drop-in must be removed on uninstall")
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("the ~/.local/bin symlink must be removed on uninstall")
	}
	if b3, _ := os.ReadFile(filepath.Join(home, ".bashrc")); strings.Contains(string(b3), dir) {
		t.Error("rc cleanup failed")
	}
}
