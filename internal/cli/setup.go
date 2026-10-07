package cli

import (
	"fmt"

	"winc/internal/agent"
	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

// cmdSetup is the first-run wizard: detect -> engine -> model -> Claude Code ->
// PATH. Idempotent; safe to re-run.
func cmdSetup() int {
	cfg := loadConfig() // writes default winc.toml if missing
	cat := catalog.Load(cfg.CustomModels)

	ui.Step(1, 7, "Detecting hardware")
	hw := platform.DetectHardware()
	ui.Say("  OS=%s/%s  RAM=%d MB  GPU=%s %s  VRAM=%d MB", hw.OS, hw.Arch, hw.RAMMB, hw.GPUVendor, hw.GPUName, hw.VRAMMB)
	tier := catalog.VramTier(hw.MemoryBudgetMB())
	ui.Say("  backend=%s  memory budget=%d MB  ->  tier '%s'", platform.DefaultBackend(hw), hw.MemoryBudgetMB(), tier)

	ui.Step(2, 7, "Config")
	ui.Good("single config file: %s", paths.ConfigPath())
	ui.Say("  reasoning mode: %s   (edit winc.toml to change)", cfg.Reasoning.Mode)

	ui.Step(3, 7, "Engine: llama.cpp")
	serverBin, err := engine.AcquireLlama(hw)
	if err != nil {
		ui.Err("could not get llama.cpp: %v", err)
		return 1
	}
	ui.Good("llama-server: %s", serverBin)

	ui.Step(4, 7, "Multi-model router: llama-swap")
	if _, err := engine.AcquireSwap(hw); err != nil {
		ui.Warn("llama-swap optional; skipped (%v)", err)
	}

	ui.Step(5, 7, "Model")
	if anyModelDownloaded(cfg) {
		ui.Good("models already present in %s", modelsDir(cfg))
	} else if def := recommendModel(cat, hw.MemoryBudgetMB()); def != nil {
		if !ui.Interactive() {
			ui.Say("  recommended model for tier '%s': %s (%s) - not downloading from a non-interactive run; use:  winc -d %s", tier, def.Alias, def.Size, def.Alias)
		} else if ui.Confirm(fmt.Sprintf("Download recommended model %s (%s) for tier '%s'?", def.Alias, def.Size, tier), true) {
			if _, err := download.HFDownload(def.Repo, def.File, modelsDir(cfg), cfg.HuggingFace.Token); err != nil {
				ui.Warn("model download failed: %v", err)
			} else {
				ui.Good("downloaded %s", def.Alias)
				if err := config.UpdateDefaultModel(def.Alias); err == nil {
					ui.Say("  set %s as default model in winc.toml", def.Alias)
				}
			}
		}
	} else {
		ui.Say("  no catalogue model for tier %q; use 'winc -d <alias>'", tier)
	}

	ui.Step(6, 7, "Coding agent: Claude Code")
	// The agent is the other half of the product; a fresh machine used to leave
	// this wizard with nothing to launch. Native install, same release bucket and
	// checksums as the official one-liners, offered -- never forced.
	if p, ok := agent.Resolve("claude"); ok {
		if v := claudeVersion(p); v != "" {
			ui.Good("Claude Code: %s (%s)", p, v)
		} else {
			ui.Good("Claude Code: %s", p)
		}
	} else if ui.Interactive() && ui.Confirm("Claude Code is not installed. Install it now (official release, sha256-verified, ~250 MB)?", true) {
		installClaude("")
	} else {
		ui.Say("  not installed - any time:  winc install claude   (or 'winc -s claude <model>' offers it)")
	}

	ui.Step(7, 7, "PATH")
	dir := paths.InstallDir()
	// Gate on the LIVE PATH, not the recorded rc entries: an install that wrote
	// .bashrc before fish support existed "looks recorded" forever while fish
	// users still can't run winc. AddToPath is idempotent and fills exactly the
	// gaps (fish conf.d, ~/.local/bin symlink, missing rc lines).
	if liveOnPath(dir) {
		ui.Good("PATH already includes %s", dir)
	} else if err := platform.AddToPath(dir); err != nil {
		ui.Warn("could not add to PATH: %v", err)
	} else {
		ui.Good("added to PATH (bash/zsh/profile, fish, ~/.local/bin): %s - open a NEW terminal to use 'winc' globally", dir)
	}

	ui.Say("")
	ui.Good("Setup complete.  Try:  winc ls    then    winc -s claude <model>")
	return 0
}
