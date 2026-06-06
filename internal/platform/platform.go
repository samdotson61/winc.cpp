// Package platform isolates every OS-specific operation behind one small surface
// so the rest of winc.cpp is portable. Per-OS implementations live in
// windows.go / linux.go / darwin.go (build-tagged); shared logic lives here.
package platform

import (
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Hardware describes the machine for model sizing + backend selection.
type Hardware struct {
	OS        string
	Arch      string
	RAMMB     int
	GPUVendor string // nvidia | amd | intel | apple | none
	GPUName   string
	VRAMMB    int
	Unified   bool // Apple Silicon: GPU shares system RAM
	CudaMajor int  // max CUDA version the NVIDIA driver supports (0 if unknown)
	CudaMinor int
}

// MemoryBudgetMB is the memory to size model recommendations against.
//
// Only Apple Silicon's *unified* memory legitimately lets the GPU use system RAM,
// so it's the only case that counts RAM. On Windows/Linux a discrete (or
// integrated) GPU is bounded by its *dedicated* VRAM - Windows "shared GPU
// memory" / system RAM is far too slow to treat as VRAM, so we never fall back to
// it (a 2 GB GPU with 16 GB RAM must not be sized as 16 GB). When VRAM is unknown
// it stays 0, yielding the smallest tier, which is the safe default. A CPU-only
// machine is sized by RAM but capped, since large models are impractical on CPU.
func (h Hardware) MemoryBudgetMB() int {
	if h.Unified {
		return h.RAMMB
	}
	if h.GPUVendor != "" && h.GPUVendor != "none" {
		return h.VRAMMB // dedicated VRAM only; never system/shared RAM
	}
	if h.RAMMB > 8192 {
		return 8192 // CPU-only: cap so we don't recommend huge, slow models
	}
	return h.RAMMB
}

// AssetSpec identifies the prebuilt archive(s) to fetch for an engine backend.
type AssetSpec struct {
	Backend string
	Files   []string // archive filenames (main [+ runtime, e.g. CUDA cudart])
	Archive string   // "zip" | "tar.gz"
}

// DetectHardware combines OS-specific probes with the shared nvidia-smi check.
func DetectHardware() Hardware {
	hw := Hardware{OS: runtime.GOOS, Arch: runtime.GOARCH}
	hw.RAMMB = detectRAMMB()
	detectGPU(&hw)
	return hw
}

// detectNvidia is shared (nvidia-smi exists on Windows + Linux). Returns true on success.
func detectNvidia(hw *Hardware) bool {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total,name", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(out2first(string(out)))
	parts := strings.SplitN(line, ",", 2)
	if len(parts) < 2 {
		return false
	}
	mb, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return false
	}
	hw.GPUVendor = "nvidia"
	hw.VRAMMB = mb
	hw.GPUName = strings.TrimSpace(parts[1])
	detectCuda(hw)
	return true
}

// detectCuda reads the max CUDA version the driver supports from nvidia-smi's
// header ("CUDA Version: 12.4"). Used to pick a matching prebuilt CUDA build.
func detectCuda(hw *Hardware) {
	out, err := exec.Command("nvidia-smi").Output()
	if err != nil {
		return
	}
	// Drivers vary: "CUDA Version: 12.4" (older) and "CUDA UMD Version: 13.3" (newer).
	m := regexp.MustCompile(`(?i)cuda[a-z ]*version:\s*(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if len(m) == 3 {
		hw.CudaMajor, _ = strconv.Atoi(m[1])
		hw.CudaMinor, _ = strconv.Atoi(m[2])
	}
}

func out2first(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// DefaultBackend chooses a llama.cpp backend from detected hardware + OS.
func DefaultBackend(hw Hardware) string {
	switch hw.GPUVendor {
	case "apple":
		return "metal"
	case "nvidia":
		return "cuda"
	case "amd":
		if hw.OS == "linux" {
			return "rocm"
		}
		return "vulkan"
	case "intel":
		return "vulkan"
	default:
		return "cpu"
	}
}
