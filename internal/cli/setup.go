package cli

import (
	"fmt"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

// cmdSetup is the first-run wizard: detect -> engine -> model -> PATH. Idempotent;
// safe to re-run.
func cmdSetup() int {
	cfg := loadConfig() // writes default winc.toml if missing
	cat := catalog.Load(cfg.CustomModels)

	ui.Step(1, 6, "Detecting hardware")
	hw := platform.DetectHardware()
	ui.Say("  OS=%s/%s  RAM=%d MB  GPU=%s %s  VRAM=%d MB", hw.OS, hw.Arch, hw.RAMMB, hw.GPUVendor, hw.GPUName, hw.VRAMMB)
	tier := catalog.VramTier(hw.MemoryBudgetMB())
	ui.Say("  backend=%s  memory budget=%d MB  ->  tier '%s'", platform.DefaultBackend(hw), hw.MemoryBudgetMB(), tier)

	ui.Step(2, 6, "Config")
	ui.Good("single config file: %s", paths.ConfigPath())
	ui.Say("  reasoning mode: %s   (edit winc.toml to change)", cfg.Reasoning.Mode)

	ui.Step(3, 6, "Engine: llama.cpp")
	serverBin, err := engine.AcquireLlama(hw)
	if err != nil {
		ui.Err("could not get llama.cpp: %v", err)
		return 1
	}
	ui.Good("llama-server: %s", serverBin)

	ui.Step(4, 6, "Multi-model router: llama-swap")
	if _, err := engine.AcquireSwap(hw); err != nil {
		ui.Warn("llama-swap optional; skipped (%v)", err)
	}

	ui.Step(5, 6, "Model")
	if anyModelDownloaded(cfg) {
		ui.Good("models already present in %s", modelsDir(cfg))
	} else if def := firstInTier(cat, tier); def != nil {
		if ui.Confirm(fmt.Sprintf("Download recommended model %s (%s) for tier '%s'?", def.Alias, def.Size, tier), true) {
			if _, err := download.HFDownload(def.Repo, def.File, modelsDir(cfg), cfg.HuggingFace.Token); err != nil {
				ui.Warn("model download failed: %v", err)
			} else {
				ui.Good("downloaded %s", def.Alias)
				if err := config.UpdateDefaultModel(def.Alias); err == nil {
					ui.Say("  set %s as default model in winc.toml", def.Alias)
				}
				offerDraft(cfg, cat, def, false) // dense pick -> offer its speculative-decoding draft
			}
		}
	} else {
		ui.Say("  no catalogue model for tier %q; use 'winc -d <alias>'", tier)
	}

	ui.Step(6, 6, "PATH")
	dir := paths.InstallDir()
	if platform.OnPath(dir) {
		ui.Good("PATH already includes %s", dir)
	} else if err := platform.AddToPath(dir); err != nil {
		ui.Warn("could not add to PATH: %v", err)
	} else {
		ui.Good("added to user PATH: %s (open a new terminal to use 'winc' globally)", dir)
	}

	ui.Say("")
	ui.Good("Setup complete.  Try:  winc ls    then    winc -s claude <model>")
	return 0
}
