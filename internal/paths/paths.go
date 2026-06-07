// Package paths resolves all winc.cpp directories relative to the binary's own
// location, so the entire folder can be moved or renamed and everything still
// works (no baked absolute paths). Set WINC_HOME to override (dev/testing).
package paths

import (
	"os"
	"path/filepath"
)

// InstallDir is the directory the winc executable lives in.
func InstallDir() string {
	if h := os.Getenv("WINC_HOME"); h != "" {
		return h
	}
	exe, err := os.Executable()
	if err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return wd
}

// ConfigPath is the single config file: <install>/winc.toml
func ConfigPath() string { return filepath.Join(InstallDir(), "winc.toml") }

// CatalogPath is the optional on-disk model catalogue: <install>/catalog.json.
// When present it overrides the catalogue embedded in the binary; `winc update`
// refreshes it, so prebuilt-binary users get new models without rebuilding.
func CatalogPath() string { return filepath.Join(InstallDir(), "catalog.json") }

// BinDir holds the engine binaries (llama-server, llama-swap): <install>/bin
func BinDir() string { return filepath.Join(InstallDir(), "bin") }

// ClaudeLocalDir is the isolated Claude Code config for the local instance.
func ClaudeLocalDir() string { return filepath.Join(InstallDir(), ".claude-local") }

// LlamaSwapYAML is the generated llama-swap config (multi-model mode).
func LlamaSwapYAML() string { return filepath.Join(InstallDir(), "llama-swap.yaml") }

// LlamaDir is the source checkout when building llama.cpp from source.
func LlamaDir() string { return filepath.Join(InstallDir(), "llama.cpp") }

// ModelsDir returns the configured models dir or the default <install>/models.
func ModelsDir(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(InstallDir(), "models")
}
