// Package platform isolates every OS-specific operation behind one small surface
// so the rest of winc.cpp is portable. Per-OS implementations live in
// windows.go / linux.go / darwin.go (build-tagged); shared logic lives here.
package platform

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"winc/internal/paths"
)

// Hardware describes the machine for model sizing + backend selection.
type Hardware struct {
	OS        string
	Arch      string
	RAMMB     int
	GPUVendor string // nvidia | amd | intel | apple | none
	GPUName   string
	VRAMMB    int         // total across all detected GPUs (dedicated only)
	GPUs      []GPUDevice // per-GPU detail (nvidia-smi); empty when only a single total is known
	Unified   bool        // Apple Silicon: GPU shares system RAM
	CudaMajor int         // max CUDA version the NVIDIA driver supports (0 if unknown)
	CudaMinor int
}

// GPUDevice is one detected GPU. FreeMB is a launch-time snapshot used to weight
// the multi-GPU tensor split toward the emptier card; 0 means unknown.
type GPUDevice struct {
	Name    string
	TotalMB int
	FreeMB  int
}

// MultiGPU reports whether more than one usable GPU was detected.
func (h Hardware) MultiGPU() bool { return len(h.GPUs) > 1 }

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
		// Apple Silicon shares RAM between the OS, apps, and the GPU, and Metal's GPU
		// working set is only a fraction of unified memory -- a model can't use all of
		// it. Budget ~72% so the model + KV fit the working set with OS headroom; this
		// stops winc recommending a model too big to load (e.g. a 22 GB model on a
		// 24 GB Mac, which barely loads and leaves no room for context).
		return h.RAMMB * 72 / 100
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
// Always a FULL probe; it also refreshes the identity cache that
// DetectHardwareCached reads, so `winc detect` / doctor / setup are how a
// hardware swap becomes visible to the fast path.
func DetectHardware() Hardware {
	hw := Hardware{OS: runtime.GOOS, Arch: runtime.GOARCH}
	hw.RAMMB = detectRAMMB()
	detectGPU(&hw)
	saveHWCache(hw)
	return hw
}

// hwCache is the persisted slow-to-detect hardware IDENTITY: vendor, name,
// dedicated VRAM total, unified flag, CUDA version. On Windows the non-NVIDIA
// probes are PowerShell invocations -- seconds per launch, every launch, for
// facts that change only on a hardware or driver swap.
type hwCache struct {
	Vendor    string    `json:"vendor"`
	Name      string    `json:"name"`
	VRAMMB    int       `json:"vram_mb"`
	Unified   bool      `json:"unified"`
	CudaMajor int       `json:"cuda_major"`
	CudaMinor int       `json:"cuda_minor"`
	SavedAt   time.Time `json:"saved_at"`
}

func hwCachePath() string { return filepath.Join(paths.InstallDir(), ".winc-hw") }

// hwCacheMaxAge bounds how long a non-verifiable identity (no live probe to
// cross-check, i.e. non-NVIDIA) is trusted before a full re-detect.
const hwCacheMaxAge = 7 * 24 * time.Hour

func saveHWCache(hw Hardware) {
	c := hwCache{Vendor: hw.GPUVendor, Name: hw.GPUName, VRAMMB: hw.VRAMMB,
		Unified: hw.Unified, CudaMajor: hw.CudaMajor, CudaMinor: hw.CudaMinor, SavedAt: time.Now()}
	if out, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(hwCachePath(), out, 0o644)
	}
}

func loadHWCache() (hwCache, bool) {
	var c hwCache
	data, err := os.ReadFile(hwCachePath())
	if err != nil || json.Unmarshal(data, &c) != nil || c.Vendor == "" {
		return hwCache{}, false
	}
	return c, true
}

// DetectHardwareCached is DetectHardware for the launch hot path: the stable
// identity comes from .winc-hw, and only the volatile parts are probed live --
// per-GPU free memory (one nvidia-smi call, needed fresh anyway) and total RAM
// (an in-process API call). Identity drift self-heals: on NVIDIA the live
// memory probe's totals must match the cache (a swapped/removed card forces a
// full re-detect); elsewhere the cache expires after hwCacheMaxAge. Delete
// .winc-hw or run `winc detect` after a hardware change to refresh immediately.
func DetectHardwareCached() Hardware {
	c, ok := loadHWCache()
	if !ok {
		return DetectHardware()
	}
	hw := Hardware{OS: runtime.GOOS, Arch: runtime.GOARCH, RAMMB: detectRAMMB()}
	if c.Vendor == "nvidia" {
		gpus := ProbeGPUFree()
		total := 0
		for _, g := range gpus {
			total += g.TotalMB
		}
		if len(gpus) == 0 || total != c.VRAMMB {
			return DetectHardware() // cards/driver changed -> probe for real
		}
		hw.GPUVendor, hw.GPUs, hw.VRAMMB, hw.GPUName = "nvidia", gpus, total, c.Name
		hw.CudaMajor, hw.CudaMinor = c.CudaMajor, c.CudaMinor
		return hw
	}
	if time.Since(c.SavedAt) > hwCacheMaxAge {
		return DetectHardware()
	}
	hw.GPUVendor, hw.GPUName, hw.VRAMMB, hw.Unified = c.Vendor, c.Name, c.VRAMMB, c.Unified
	hw.CudaMajor, hw.CudaMinor = c.CudaMajor, c.CudaMinor
	return hw
}

// ProbeGPUFree re-reads just the per-GPU memory snapshot: one nvidia-smi query,
// none of the RAM/vendor/CUDA probes a full DetectHardware pays for. For poll
// loops (VRAM drain waits) and post-load leftover checks that only care how free
// VRAM moved. Empty on non-NVIDIA machines (no per-GPU data), matching the
// Hardware.GPUs contract.
func ProbeGPUFree() []GPUDevice {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total,memory.free,name", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSmi(string(out))
}

// detectNvidia is shared (nvidia-smi exists on Windows + Linux). Returns true on success.
// All GPUs are detected, not just the first -- a second card extends both the memory
// budget (VRAMMB is the sum) and the launch flags (layers split across the cards).
func detectNvidia(hw *Hardware) bool {
	gpus := ProbeGPUFree()
	if len(gpus) == 0 {
		return false
	}
	hw.GPUVendor = "nvidia"
	hw.GPUs = gpus
	names := make([]string, len(gpus))
	for i, g := range gpus {
		hw.VRAMMB += g.TotalMB
		names[i] = g.Name
	}
	hw.GPUName = strings.Join(names, " + ")
	detectCuda(hw)
	return true
}

// parseNvidiaSmi parses `--query-gpu=memory.total,memory.free,name` CSV output:
// one line per GPU, values in MiB without units, name last (names contain no commas).
func parseNvidiaSmi(out string) []GPUDevice {
	var gpus []GPUDevice
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ",", 3)
		if len(parts) < 3 {
			continue
		}
		total, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || total <= 0 {
			continue
		}
		free, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || free < 0 {
			free = 0
		}
		gpus = append(gpus, GPUDevice{Name: strings.TrimSpace(parts[2]), TotalMB: total, FreeMB: free})
	}
	return gpus
}

// Drivers vary: "CUDA Version: 12.4" (older) and "CUDA UMD Version: 13.3" (newer).
var cudaVersionRe = regexp.MustCompile(`(?i)cuda[a-z ]*version:\s*(\d+)\.(\d+)`)

var (
	cudaOnce             sync.Once
	cudaMajor, cudaMinor int
)

// detectCuda reads the max CUDA version the driver supports from nvidia-smi's
// header ("CUDA Version: 12.4"). Used to pick a matching prebuilt CUDA build.
// The version can't change while winc runs, but a launch detects hardware
// several times (sizing, the context ladder, team leftover) -- so the extra
// nvidia-smi invocation this needs is paid once per process.
func detectCuda(hw *Hardware) {
	cudaOnce.Do(func() {
		out, err := exec.Command("nvidia-smi").Output()
		if err != nil {
			return
		}
		if m := cudaVersionRe.FindStringSubmatch(string(out)); len(m) == 3 {
			cudaMajor, _ = strconv.Atoi(m[1])
			cudaMinor, _ = strconv.Atoi(m[2])
		}
	})
	hw.CudaMajor, hw.CudaMinor = cudaMajor, cudaMinor
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
