package engine

import (
	"path/filepath"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

// MoEPackPlan: window first, experts take the leftover, --n-cpu-moe for the
// rest. Sizes seeded through the expert cache (no multi-GB fixtures -- NTFS
// allocates truncated files for real; the v1.14.3 CI-disk lesson).
func TestMoEPackPlan(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Fake-35B-A3B-Q4_K_M.gguf")
	moeExpertCache.Store(p, moeExpertStats{TotalBytes: int64(19126) << 20, Layers: 41})
	expertCountCache.Store(p, 64) // authoritative MoE (skip the filename fallback)
	const modelMB, vram, ctx = 21613, 16303, 32768

	cfg := config.Defaults() // cpu_moe auto, cache auto, flash on
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: vram, GPUs: []platform.GPUDevice{{TotalMB: vram}}}

	n, spill, ok := MoEPackPlan(&cfg, hw, p, modelMB, ctx)
	if !ok {
		t.Fatal("pack plan should apply: auto MoE offload with leftover VRAM")
	}
	// base 2487 + kv 512 + reserve 1536 + safety 512 -> ~11.2 GB leftover
	// -> 24 layers on GPU, 17 on CPU (466 MB/layer).
	if n < 14 || n > 20 {
		t.Errorf("cpuLayers = %d, want ~17 for these sizes", n)
	}
	if want := n * (19126 / 41); spill != want {
		t.Errorf("spill = %d, want %d", spill, want)
	}
	// A larger window packs fewer experts but still packs.
	nBig, _, okBig := MoEPackPlan(&cfg, hw, p, modelMB, 262144)
	if !okBig || nBig <= n {
		t.Errorf("bigger window must leave fewer GPU layers: got cpu=%d (base %d), ok=%v", nBig, n, okBig)
	}
	// Explicit user setting is never repacked.
	cfg.Performance.CpuMoe = "on"
	if _, _, ok := MoEPackPlan(&cfg, hw, p, modelMB, ctx); ok {
		t.Error("explicit cpu_moe=on must keep full offload")
	}
	cfg.Performance.CpuMoe = "auto"
	// No leftover for even one layer -> no plan (plain --cpu-moe).
	tiny := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 5000, GPUs: []platform.GPUDevice{{TotalMB: 5000}}}
	if _, _, ok := MoEPackPlan(&cfg, tiny, p, modelMB, ctx); ok {
		t.Error("5 GB card has no expert leftover; plan must not apply")
	}
	// A model that fully fits never reaches the offload branch at all.
	small := filepath.Join(t.TempDir(), "Small-A3B-Q4_K_M.gguf")
	moeExpertCache.Store(small, moeExpertStats{TotalBytes: int64(3000) << 20, Layers: 30})
	expertCountCache.Store(small, 64)
	if _, _, ok := MoEPackPlan(&cfg, hw, small, 4000, ctx); ok {
		t.Error("fully-fitting MoE must not plan a pack")
	}
}
