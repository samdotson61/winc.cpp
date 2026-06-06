// Package platform isolates every OS-specific operation behind one small surface
// so the rest of winc.cpp is portable. Per-OS implementations live in
// windows.go / linux.go / darwin.go (build-tagged); shared logic lives here.
package platform

import (
	"os/exec"
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
}

// MemoryBudgetMB is the memory to size models against: discrete VRAM for a
// dedicated GPU, otherwise system RAM (unified-memory Macs, or CPU-only).
func (h Hardware) MemoryBudgetMB() int {
	if h.Unified {
		return h.RAMMB
	}
	if h.VRAMMB > 0 {
		return h.VRAMMB
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
	return true
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
