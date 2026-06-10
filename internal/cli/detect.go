package cli

import (
	"path/filepath"
	"strconv"
	"strings"

	"winc/internal/catalog"
	"winc/internal/engine"
	"winc/internal/platform"
	"winc/internal/ui"
)

// sizeStrToMB parses a catalogue size like "14.3 GB" into megabytes (0 if unparsable).
func sizeStrToMB(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := 1024.0
	if strings.HasSuffix(s, "GB") {
		s = strings.TrimSuffix(s, "GB")
	} else if strings.HasSuffix(s, "MB") {
		s, mult = strings.TrimSuffix(s, "MB"), 1.0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(v * mult)
}

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
		if hw.MultiGPU() {
			for i, g := range hw.GPUs {
				ui.Say("      gpu %d     : %s  (%d MB, %d MB free)", i, g.Name, g.TotalMB, g.FreeMB)
			}
			ui.Say("      the engine fits layers across all %d GPUs by free VRAM at load", len(hw.GPUs))
		}
	}
	if hw.CudaMajor > 0 {
		ui.Say("  CUDA (driver) : %d.%d", hw.CudaMajor, hw.CudaMinor)
	}
	ui.Say("  Engine backend: %s", platform.DefaultBackend(hw))
	ui.Say("  Memory budget : %d MB  ->  tier '%s'", hw.MemoryBudgetMB(), tier)
	if def := firstInTier(cat, tier); def != nil {
		ui.Say("  Recommended   : %s (%s)", def.Alias, def.Size)

		// Show what winc would resolve for that model (real on-disk size if it's
		// already downloaded, else the catalogue estimate).
		modelMB := sizeStrToMB(def.Size)
		approx := "~"
		if p := filepath.Join(modelsDir(cfg), def.File); fileExists(p) {
			if m := engine.FileMB(p); m > 0 {
				modelMB, approx = m, ""
			}
		}
		ctx, moe := engine.PlanForModel(cfg, hw, def.File, modelMB)
		ui.Say("  Auto context  : %d tokens (for %s%d MB model)", ctx, approx, modelMB)
		switch {
		case moe == "all":
			ui.Say("  MoE offload   : yes (experts -> RAM; attention on GPU)")
		case moe != "":
			ui.Say("  MoE offload   : %s expert layers -> RAM", moe)
		case engine.IsMoEFile(def.File):
			ui.Say("  MoE offload   : no (model fits VRAM; runs fully on GPU)")
		}
		if def.Mtp != "" {
			if v := cat.Find(def.Mtp); v != nil {
				ui.Say("  Faster variant: %s (MTP; winc -d %s)", v.Alias, v.Alias)
			}
		}
		if def.MtpHead != "" {
			ui.Say("  MTP drafter   : separate head file, auto-paired at launch ('winc -d %s' fetches it)", def.Alias)
		}
	}
	ui.Say("")
	return 0
}
