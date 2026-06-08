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
