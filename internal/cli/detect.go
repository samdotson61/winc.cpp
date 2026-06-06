package cli

import (
	"strings"

	"winc/internal/catalog"
	"winc/internal/platform"
	"winc/internal/ui"
)

// cmdDetect prints what winc detects about the machine and which tier/model it
// would recommend - handy for diagnosing GPU/VRAM detection.
func cmdDetect() int {
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)
	hw := platform.DetectHardware()
	tier := catalog.VramTier(hw.MemoryBudgetMB())

	gpu := hw.GPUVendor
	if gpu == "" {
		gpu = "none"
	}
	ui.Say("")
	ui.Say("Detected hardware:")
	ui.Say("  OS / arch     : %s / %s", hw.OS, hw.Arch)
	ui.Say("  System RAM    : %d MB", hw.RAMMB)
	ui.Say("  GPU           : %s  (%s)", gpu, strings.TrimSpace(hw.GPUName))
	if hw.Unified {
		ui.Say("  Memory        : %d MB unified (Apple Silicon)", hw.RAMMB)
	} else {
		ui.Say("  Dedicated VRAM: %d MB", hw.VRAMMB)
	}
	if hw.CudaMajor > 0 {
		ui.Say("  CUDA (driver) : %d.%d", hw.CudaMajor, hw.CudaMinor)
	}
	ui.Say("  Engine backend: %s", platform.DefaultBackend(hw))
	ui.Say("  Memory budget : %d MB  ->  tier '%s'", hw.MemoryBudgetMB(), tier)
	if def := firstInTier(cat, tier); def != nil {
		ui.Say("  Recommended   : %s (%s)", def.Alias, def.Size)
	}
	ui.Say("")
	return 0
}
