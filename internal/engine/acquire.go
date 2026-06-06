package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"winc/internal/download"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func llamaServerInBin() string { return findIn("llama-server", paths.BinDir()) }

// ClearBinEngine removes the engine executables from bin/ so the next Acquire
// re-downloads the latest. Shared libs are left (re-extract overwrites them).
func ClearBinEngine() {
	for _, n := range []string{"llama-server", "llama-cli", "llama-swap"} {
		if p := findIn(n, paths.BinDir()); p != "" {
			_ = os.Remove(p)
		}
	}
}

// AcquireLlama ensures a llama-server binary exists in winc's bin dir, downloading
// the best prebuilt archive for the hardware (trying backends in order, always
// ending in a CPU fallback). Idempotent: returns immediately if bin/ already has
// one. Returns the llama-server path.
func AcquireLlama(hw platform.Hardware) (string, error) {
	if p := llamaServerInBin(); p != "" {
		return p, nil
	}
	cands := LlamaCandidates(hw)
	if len(cands) == 0 {
		return "", fmt.Errorf("no prebuilt llama.cpp asset for %s/%s (build from source)", hw.OS, hw.Arch)
	}
	tmp := filepath.Join(paths.InstallDir(), ".winc-dl")
	_ = os.MkdirAll(tmp, 0o755)
	defer os.RemoveAll(tmp)

	var lastErr error
	for _, a := range cands {
		ui.Info("fetching llama.cpp (%s backend)...", a.Backend)
		ok := true
		for _, u := range a.URLs {
			arc := filepath.Join(tmp, filepath.Base(u))
			if err := download.Fetch(u, arc, map[string]string{"User-Agent": "winc.cpp"}, filepath.Base(u)); err != nil {
				ui.Warn("  %s: %v", a.Backend, err)
				lastErr = err
				ok = false
				break
			}
			if err := extractFlat(arc, paths.BinDir(), a.Archive); err != nil {
				ui.Warn("  extract %s: %v", a.Backend, err)
				lastErr = err
				ok = false
				break
			}
		}
		if ok {
			if p := llamaServerInBin(); p != "" {
				ui.Good("llama.cpp ready (%s backend)", a.Backend)
				return p, nil
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("archives downloaded but llama-server not found inside")
	}
	return "", lastErr
}

// AcquireSwap ensures the llama-swap binary exists in bin/, downloading the
// prebuilt. Idempotent.
func AcquireSwap(hw platform.Hardware) (string, error) {
	if p := LlamaSwapPath(); p != "" {
		return p, nil
	}
	url, archive, ok := SwapAsset(hw)
	if !ok {
		return "", fmt.Errorf("no llama-swap asset for %s/%s", hw.OS, hw.Arch)
	}
	tmp := filepath.Join(paths.InstallDir(), ".winc-dl")
	_ = os.MkdirAll(tmp, 0o755)
	defer os.RemoveAll(tmp)
	arc := filepath.Join(tmp, filepath.Base(url))
	ui.Info("fetching llama-swap...")
	if err := download.Fetch(url, arc, map[string]string{"User-Agent": "winc.cpp"}, filepath.Base(url)); err != nil {
		return "", err
	}
	if err := extractFlat(arc, paths.BinDir(), archive); err != nil {
		return "", err
	}
	p := LlamaSwapPath()
	if p == "" {
		return "", fmt.Errorf("llama-swap not found after extraction")
	}
	ui.Good("llama-swap ready")
	return p, nil
}
