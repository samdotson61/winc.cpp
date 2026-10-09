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

// A second directory (Claude Code's ~/.local/bin after `winc install claude`)
// must be ADDED beside winc's own entries, never replace them -- the fish
// drop-in used to be rewritten whole -- and must not get the winc symlink.
func TestAddDirToPathBesideWinc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin")
	winc := "/opt/winc"
	claude := filepath.Join(home, ".local", "bin")
	if err := AddToPath(winc); err != nil {
		t.Fatal(err)
	}
	if err := AddDirToPath(claude); err != nil {
		t.Fatal(err)
	}
	fish, _ := os.ReadFile(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish"))
	if !strings.Contains(string(fish), winc) || !strings.Contains(string(fish), claude) {
		t.Errorf("fish drop-in must hold BOTH dirs:\n%s", fish)
	}
	rc, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !strings.Contains(string(rc), winc) || !strings.Contains(string(rc), claude) {
		t.Errorf(".zshrc must hold BOTH dirs:\n%s", rc)
	}
	if !OnPath(claude) || !OnPath(winc) {
		t.Error("both dirs must now count as on PATH")
	}
	if err := AddDirToPath(claude); err != nil {
		t.Fatal(err)
	}
	fish2, _ := os.ReadFile(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish"))
	if strings.Count(string(fish2), claude) != strings.Count(string(fish), claude) {
		t.Error("AddDirToPath must be idempotent in the fish drop-in")
	}
	// winc's symlink still points at winc, and ~/.local/bin never gets a
	// self-referential one.
	link := filepath.Join(home, ".local", "bin", "winc")
	if cur, err := os.Readlink(link); err != nil || cur != filepath.Join(winc, "winc") {
		t.Errorf("winc symlink disturbed: %q err=%v", cur, err)
	}
	if err := AddToPath(claude); err != nil { // winc itself living in ~/.local/bin
		t.Fatal(err)
	}
	if cur, _ := os.Readlink(link); cur == link {
		t.Error("~/.local/bin/winc must never be a symlink to itself")
	}
}

// Uninstalling winc must take only winc's own PATH entries: Claude Code's
// ~/.local/bin block (recorded by `winc install claude`) stays in the rc files
// AND in the fish drop-in, and is still recognized as recorded afterwards.
func TestRemoveFromPathLeavesOtherDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin")
	winc, claude := "/opt/winc", filepath.Join(home, ".local", "bin")
	if err := AddToPath(winc); err != nil {
		t.Fatal(err)
	}
	if err := AddDirToPath(claude); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFromPath(winc); err != nil {
		t.Fatal(err)
	}
	rc, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	if strings.Contains(string(rc), winc) {
		t.Errorf("winc's export survived:\n%s", rc)
	}
	if !strings.Contains(string(rc), claude) || strings.Count(string(rc), pathMarker) != 1 {
		t.Errorf("Claude Code's block (marker + export) must survive intact:\n%s", rc)
	}
	fish, err := os.ReadFile(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish"))
	if err != nil || !strings.Contains(string(fish), claude) || strings.Contains(string(fish), winc) {
		t.Errorf("fish drop-in must keep Claude Code's block only: err=%v\n%s", err, fish)
	}
	if !OnPath(claude) || OnPath(winc) {
		t.Error("recorded state wrong after removal")
	}
	if err := AddDirToPath(claude); err != nil { // still idempotent
		t.Fatal(err)
	}
	if rc2, _ := os.ReadFile(filepath.Join(home, ".bashrc")); strings.Count(string(rc2), pathMarker) != 1 {
		t.Error("re-adding the surviving dir must not duplicate it")
	}
	if err := RemoveFromPath(claude); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "fish", "conf.d", "winc.fish")); err == nil {
		t.Error("an empty drop-in must be deleted")
	}
}
