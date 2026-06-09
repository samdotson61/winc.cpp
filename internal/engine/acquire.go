package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"winc/internal/download"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func llamaServerInBin() string { return findIn("llama-server", paths.BinDir()) }

func backendMarker() string { return filepath.Join(paths.BinDir(), ".winc-backend") }

// CurrentBackend returns the backend recorded for the installed engine ("" if unknown).
func CurrentBackend() string {
	b, err := os.ReadFile(backendMarker())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeBackend(name string) { _ = os.WriteFile(backendMarker(), []byte(name+"\n"), 0o644) }

// ClearBinEngine removes the engine executables and backend marker from bin/ so
// the next Acquire re-downloads. Shared libs are overwritten on re-extract.
func ClearBinEngine() {
	for _, n := range []string{"llama-server", "llama-cli", "llama-swap"} {
		if p := findIn(n, paths.BinDir()); p != "" {
			_ = os.Remove(p)
		}
	}
	_ = os.Remove(backendMarker())
}

// AcquireLlama ensures a llama-server exists in bin/, downloading the best
// prebuilt for the hardware. Idempotent. Wrapper kept for setup/update.
func AcquireLlama(hw platform.Hardware) (string, error) {
	p, _, err := AcquireLlamaExcluding(hw, nil)
	return p, err
}

// AcquireLlamaExcluding downloads the best prebuilt backend NOT in exclude,
// returning (serverPath, backendName). If bin/ already has a server whose backend
// isn't excluded, it's reused. The chosen backend is recorded in a marker so a
// later runtime failure can skip it.
func AcquireLlamaExcluding(hw platform.Hardware, exclude map[string]bool) (string, string, error) {
	if p := llamaServerInBin(); p != "" {
		cur := CurrentBackend()
		if !exclude[cur] {
			return p, cur, nil
		}
	}
	cands := LlamaCandidates(hw)
	if len(cands) == 0 {
		return "", "", fmt.Errorf("no prebuilt llama.cpp asset for %s/%s (build from source)", hw.OS, hw.Arch)
	}
	tmp := filepath.Join(paths.InstallDir(), ".winc-dl")
	_ = os.MkdirAll(tmp, 0o755)
	defer os.RemoveAll(tmp)

	var lastErr error
	for _, a := range cands {
		if exclude[a.Backend] {
			continue
		}
		ClearBinEngine()
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
			if err := verifyArchive(arc, a.Digests); err != nil {
				ui.Warn("  %s: %v", a.Backend, err)
				_ = os.Remove(arc) // never resume or reuse a bad archive
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
				writeBackend(a.Backend)
				ui.Good("llama.cpp ready (%s backend)", a.Backend)
				return p, a.Backend, nil
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable llama.cpp backend (all excluded or downloads failed)")
	}
	return "", "", lastErr
}

// AcquireSwap ensures the llama-swap binary exists in bin/, downloading the
// prebuilt. Idempotent.
func AcquireSwap(hw platform.Hardware) (string, error) {
	if p := LlamaSwapPath(); p != "" {
		return p, nil
	}
	url, archive, digests, ok := SwapAsset(hw)
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
	if err := verifyArchive(arc, digests); err != nil {
		_ = os.Remove(arc) // never resume or reuse a bad archive
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

// verifyArchive checks a downloaded archive against its GitHub-published sha256.
// A missing digest (offline tag fallback / older release without digests) is not an
// error -- winc proceeds and says so. A MISMATCH is a hard error: a corrupt or
// tampered archive must never be extracted into bin/.
func verifyArchive(path string, digests map[string]string) error {
	want := digests[filepath.Base(path)]
	if want == "" {
		ui.Dim("  (no published digest for %s - skipping verification)", filepath.Base(path))
		return nil
	}
	want = strings.TrimPrefix(want, "sha256:")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s (corrupt or tampered download)", filepath.Base(path), got, want)
	}
	return nil
}
