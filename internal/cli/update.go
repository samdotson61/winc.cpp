package cli

import (
	"os"
	"path/filepath"

	"winc/internal/catalog"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func cmdCheck() int {
	ui.Say("")
	ui.Say("Checking for updates...")
	ui.Good("llama.cpp latest : %s", engine.LatestLlamaTag())
	ui.Good("llama-swap latest: %s", engine.LatestSwapTag())
	if p := engine.LlamaServerPath(); p != "" {
		ui.Info("engine installed: %s", p)
	} else {
		ui.Warn("engine not installed - run 'winc setup'")
	}
	src := "built-in"
	if _, err := os.Stat(paths.CatalogPath()); err == nil {
		src = "updated, " + paths.CatalogPath()
	}
	ui.Info("model catalog   : %d models (%s)", len(catalog.Load(nil).Models), src)
	if _, err := os.Stat(filepath.Join(paths.InstallDir(), ".git")); err == nil {
		ui.Info("winc source is a git clone - 'winc update' will also pull it")
	}
	ui.Say("Run 'winc update' to refresh engine binaries + model catalog to the latest.")
	ui.Say("")
	return 0
}

func cmdUpdate() int {
	hw := platform.DetectHardware()
	if _, err := os.Stat(filepath.Join(paths.InstallDir(), ".git")); err == nil {
		ui.Info("updating winc source (git pull)...")
		_ = execInherit("git", "-C", paths.InstallDir(), "pull", "--ff-only").Run()
	}
	ui.Info("refreshing model catalog...")
	before := len(catalog.Load(nil).Models)
	if total, err := catalog.Update(); err != nil {
		ui.Warn("catalog refresh skipped: %v (keeping current %d models)", err, before)
	} else if total != before {
		ui.Good("catalog updated: %d models (was %d)", total, before)
	} else {
		ui.Good("catalog up to date (%d models)", total)
	}

	ui.Info("refreshing engine binaries to latest...")
	engine.ClearBinEngine()
	if _, err := engine.AcquireLlama(hw); err != nil {
		ui.Err("engine update failed: %v", err)
		return 1
	}
	_, _ = engine.AcquireSwap(hw)
	ui.Good("update complete.")
	return 0
}
