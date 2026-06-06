// Package engine locates, acquires, and configures the llama.cpp / llama-swap
// binaries. Locations are searched relative to the winc install dir, so a moved
// folder still works.
package engine

import (
	"os"
	"path/filepath"
	"runtime"

	"winc/internal/paths"
)

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func findIn(name string, dirs ...string) string {
	n := exeName(name)
	for _, d := range dirs {
		p := filepath.Join(d, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func searchDirs() []string {
	return []string{
		paths.BinDir(),
		filepath.Join(paths.LlamaDir(), "build", "bin", "Release"),
		filepath.Join(paths.LlamaDir(), "build", "bin"),
	}
}

// LlamaServerPath finds llama-server in winc's bin dir or an existing source build.
func LlamaServerPath() string { return findIn("llama-server", searchDirs()...) }

// LlamaCliPath finds llama-cli similarly.
func LlamaCliPath() string { return findIn("llama-cli", searchDirs()...) }

// LlamaSwapPath finds the llama-swap binary in winc's bin dir.
func LlamaSwapPath() string { return findIn("llama-swap", paths.BinDir()) }
