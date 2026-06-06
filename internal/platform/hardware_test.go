package platform

import "testing"

func TestMemoryBudgetMB(t *testing.T) {
	cases := []struct {
		name string
		hw   Hardware
		want int
	}{
		{"apple unified counts RAM", Hardware{Unified: true, RAMMB: 65536}, 65536},
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

// The reported bug: a 2 GB-VRAM laptop with 16 GB RAM was recommended a 35B model.
// Its memory budget must stay well below the mid (16 GB) tier.
func TestLowEndLaptopNotMidTier(t *testing.T) {
	hw := Hardware{GPUVendor: "nvidia", VRAMMB: 2048, RAMMB: 16000}
	if b := hw.MemoryBudgetMB(); b >= 12000 {
		t.Fatalf("2 GB laptop budget=%d would select a 16 GB-tier model", b)
	}
}
