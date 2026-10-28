package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winc/internal/platform"
)

// Every archive winc can ask for must exist in upstream's release. The list is
// the real b11960 (v0.8.0, 2026-10-28) asset set; upstream renames toolkit
// suffixes as it moves (cuda-13.3 -> 13.4, rocm-7.2 -> 7.14 -> 10.0) and a
// stale name 404s SILENTLY into the next candidate -- CUDA-13 Windows boxes ran
// the 12.4 build for a month before the 2026-10-28 audit caught it. Refresh
// testdata/llama-assets-<tag>.txt from the release page when bumping.
func TestLlamaCandidatesExistUpstream(t *testing.T) {
	const tag = "b11960"
	data, err := os.ReadFile(filepath.Join("testdata", "llama-assets-"+tag+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			have[l] = true
		}
	}
	rel := releaseInfo{Tag: tag}
	gpu := func(os, arch, vendor string, cuda int) platform.Hardware {
		return platform.Hardware{OS: os, Arch: arch, GPUVendor: vendor, CudaMajor: cuda}
	}
	profiles := []platform.Hardware{
		gpu("windows", "amd64", "nvidia", 13), gpu("windows", "amd64", "nvidia", 12), gpu("windows", "amd64", "nvidia", 0),
		gpu("windows", "amd64", "amd", 0), gpu("windows", "amd64", "intel", 0), gpu("windows", "amd64", "none", 0),
		gpu("windows", "arm64", "qualcomm", 0), gpu("windows", "arm64", "none", 0),
		gpu("linux", "amd64", "nvidia", 13), gpu("linux", "amd64", "nvidia", 12), gpu("linux", "amd64", "nvidia", 0),
		gpu("linux", "amd64", "amd", 0), gpu("linux", "amd64", "intel", 0), gpu("linux", "amd64", "none", 0),
		gpu("linux", "arm64", "nvidia", 13), gpu("linux", "arm64", "nvidia", 0), gpu("linux", "arm64", "none", 0),
		gpu("darwin", "arm64", "apple", 0), gpu("darwin", "amd64", "none", 0),
	}
	for _, hw := range profiles {
		cands := llamaCandidatesFor(hw, rel)
		if len(cands) == 0 {
			t.Errorf("%s/%s/%s: no candidates", hw.OS, hw.Arch, hw.GPUVendor)
			continue
		}
		if cands[len(cands)-1].Backend != "cpu" && hw.OS != "darwin" {
			t.Errorf("%s/%s/%s: last candidate must be the CPU fallback, got %s", hw.OS, hw.Arch, hw.GPUVendor, cands[len(cands)-1].Backend)
		}
		for _, c := range cands {
			for _, u := range c.URLs {
				name := u[strings.LastIndex(u, "/")+1:]
				if !have[name] {
					t.Errorf("%s/%s/%s cuda%d: %s asks for %q, which %s does not ship", hw.OS, hw.Arch, hw.GPUVendor, hw.CudaMajor, c.Backend, name, tag)
				}
			}
		}
	}
	// The driver pick: a CUDA-13 driver gets the 13.4 build first, a CUDA-12
	// driver never sees it, unknown gets both newest-first.
	first := func(hw platform.Hardware) string { return llamaCandidatesFor(hw, rel)[0].Backend }
	if first(gpu("windows", "amd64", "nvidia", 13)) != "cuda-13.4" || first(gpu("linux", "amd64", "nvidia", 13)) != "cuda-13.4" {
		t.Error("a CUDA-13 driver must be offered the cuda-13.4 build first")
	}
	if first(gpu("windows", "amd64", "nvidia", 12)) != "cuda-12.4" || first(gpu("linux", "amd64", "nvidia", 12)) != "cuda-12.8" {
		t.Error("a CUDA-12 driver must be offered the cuda-12.x build first")
	}
	for _, c := range llamaCandidatesFor(gpu("windows", "amd64", "nvidia", 12), rel) {
		if strings.HasPrefix(c.Backend, "cuda-13") {
			t.Error("a CUDA-12 driver must never be offered a cuda-13 build (its PTX won't load)")
		}
	}
}
