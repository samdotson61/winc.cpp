package platform

import (
	"testing"

	"winc/internal/catalog"
)

func TestMemoryBudgetMB(t *testing.T) {
	cases := []struct {
		name string
		hw   Hardware
		want int
	}{
		{"apple unified applies GPU working-set haircut", Hardware{Unified: true, RAMMB: 65536}, 47185},
		{"nvidia 16gb uses VRAM", Hardware{GPUVendor: "nvidia", VRAMMB: 16303, RAMMB: 32000}, 16303},
		{"nvidia 2gb laptop uses VRAM not RAM", Hardware{GPUVendor: "nvidia", VRAMMB: 2048, RAMMB: 16000}, 2048},
		{"non-nvidia unknown VRAM never falls back to RAM", Hardware{GPUVendor: "intel", VRAMMB: 0, RAMMB: 16000}, 0},
		{"amd discrete known VRAM", Hardware{GPUVendor: "amd", VRAMMB: 8192, RAMMB: 32000}, 8192},
		{"cpu-only high RAM capped", Hardware{GPUVendor: "none", VRAMMB: 0, RAMMB: 32000}, 8192},
		{"cpu-only low RAM", Hardware{GPUVendor: "none", VRAMMB: 0, RAMMB: 4096}, 4096},
	}
	for _, c := range cases {
		if got := c.hw.MemoryBudgetMB(); got != c.want {
			t.Errorf("%s: MemoryBudgetMB()=%d, want %d", c.name, got, c.want)
		}
	}
}

// The reported bug: a 24 GB Mac was recommended the 22 GB (large-tier) model, which
// can't fit Metal's working set. The unified haircut must keep common Mac sizes on
// tiers whose recommended model actually loads -- 24 GB must be "mid", not "large".
func TestUnifiedMacTiers(t *testing.T) {
	cases := map[int]string{
		16384: "small", // ~16 GB
		24576: "mid",   // ~24 GB  (the reported bug -- was "large" / 22 GB model)
		32768: "large", // ~32 GB  (the 22 GB model loads here)
		98304: "xl",    // ~96 GB
	}
	for ram, want := range cases {
		b := Hardware{Unified: true, RAMMB: ram}.MemoryBudgetMB()
		if got := catalog.VramTier(b); got != want {
			t.Errorf("%d MB unified -> budget %d -> tier %q, want %q", ram, b, got, want)
		}
	}
}

// The reported bug: a 2 GB-VRAM laptop with 16 GB RAM was recommended a 35B model.
// Its memory budget must stay well below the mid (16 GB) tier.
func TestLowEndLaptopNotMidTier(t *testing.T) {
	hw := Hardware{GPUVendor: "nvidia", VRAMMB: 2048, RAMMB: 16000}
	if b := hw.MemoryBudgetMB(); b >= 12000 {
		t.Fatalf("2 GB laptop budget=%d would select a 16 GB-tier model", b)
	}
}

// Two-GPU machines: every nvidia-smi line is parsed (not just the first) and the
// budget is the SUM -- a 16 GB + 12 GB pair must reach the large (24 GB+) tier.
func TestParseNvidiaSmiMultiGPU(t *testing.T) {
	out := "16303, 15054, NVIDIA GeForce RTX 5070 Ti\r\n12288, 12113, NVIDIA GeForce RTX 3060\r\n"
	gpus := parseNvidiaSmi(out)
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d (%+v)", len(gpus), gpus)
	}
	if gpus[0].Name != "NVIDIA GeForce RTX 5070 Ti" || gpus[0].TotalMB != 16303 || gpus[0].FreeMB != 15054 {
		t.Errorf("gpu0 parsed wrong: %+v", gpus[0])
	}
	if gpus[1].Name != "NVIDIA GeForce RTX 3060" || gpus[1].TotalMB != 12288 || gpus[1].FreeMB != 12113 {
		t.Errorf("gpu1 parsed wrong: %+v", gpus[1])
	}
	hw := Hardware{GPUVendor: "nvidia", GPUs: gpus, VRAMMB: gpus[0].TotalMB + gpus[1].TotalMB}
	if !hw.MultiGPU() {
		t.Error("two GPUs should report MultiGPU")
	}
	if got := catalog.VramTier(hw.MemoryBudgetMB()); got != "large" {
		t.Errorf("16+12 GB pair -> tier %q, want large", got)
	}
}

func TestParseNvidiaSmiSingleAndGarbage(t *testing.T) {
	if g := parseNvidiaSmi("8192, 7000, NVIDIA GeForce RTX 3070\n"); len(g) != 1 || g[0].TotalMB != 8192 || g[0].FreeMB != 7000 {
		t.Errorf("single GPU parse failed: %+v", g)
	}
	if g := parseNvidiaSmi("not, csv, at all\n\n"); len(g) != 0 {
		t.Errorf("garbage should parse to no GPUs, got %+v", g)
	}
	if g := parseNvidiaSmi(""); len(g) != 0 {
		t.Errorf("empty should parse to no GPUs, got %+v", g)
	}
}
