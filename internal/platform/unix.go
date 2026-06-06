//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// ExeSuffix is empty on Unix.
func ExeSuffix() string { return "" }

// EnableVT is a no-op on Unix (ANSI works out of the box).
func EnableVT() {}

const pathMarker = "# winc.cpp PATH"

func rcFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".profile"),
	}
}

// OnPath reports whether dir is on PATH now or recorded in a shell rc file.
func OnPath(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), ":") {
		if p == dir {
			return true
		}
	}
	for _, f := range rcFiles() {
		if b, err := os.ReadFile(f); err == nil {
			if strings.Contains(string(b), pathMarker) && strings.Contains(string(b), dir) {
				return true
			}
		}
	}
	return false
}

// AddToPath appends a marked export line to the user's shell rc files (idempotent).
func AddToPath(dir string) error {
	block := "\n" + pathMarker + "\nexport PATH=\"" + dir + ":$PATH\"\n"
	for _, f := range rcFiles() {
		if b, err := os.ReadFile(f); err == nil &&
			strings.Contains(string(b), pathMarker) && strings.Contains(string(b), dir) {
			continue
		}
		fh, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_, _ = fh.WriteString(block)
		_ = fh.Close()
	}
	return nil
}

// RemoveFromPath strips the marked block from the user's shell rc files.
func RemoveFromPath(dir string) error {
	for _, f := range rcFiles() {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		var out []string
		for i := 0; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == pathMarker {
				if i+1 < len(lines) && strings.Contains(lines[i+1], dir) {
					i++ // also skip the export line
				}
				continue
			}
			out = append(out, lines[i])
		}
		_ = os.WriteFile(f, []byte(strings.Join(out, "\n")), 0o644)
	}
	return nil
}
