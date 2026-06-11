package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func cmdCheck() int {
	dir := paths.InstallDir()
	ui.Say("")
	ui.Say("Checking for updates...")
	// The three release lookups, the local engine version probe, and the git
	// upstream fetch are independent -- run them concurrently so the check waits
	// for the slowest one instead of paying for all of them in sequence.
	var wincTag, latest, swapTag, inst string
	behind := make(chan int, 1)
	clone := isGitClone(dir)
	if clone {
		go func() { behind <- gitBehindCount(dir) }()
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); wincTag = engine.LatestWincTag() }()
	go func() { defer wg.Done(); latest = engine.LatestLlamaTag() }()
	go func() { defer wg.Done(); swapTag = engine.LatestSwapTag() }()
	go func() { defer wg.Done(); inst = engine.InstalledLlamaTag() }()
	wg.Wait()
	ui.Info("winc version    : %s", Version)
	if wincTag != "" && strings.TrimPrefix(wincTag, "v") != Version {
		ui.Warn("newer winc available: %s (you have %s)", wincTag, Version)
	}
	ui.Good("llama.cpp latest : %s", latest)
	ui.Good("llama-swap latest: %s", swapTag)
	switch {
	case inst == "" && engine.LlamaServerPath() == "":
		ui.Warn("engine not installed - run 'winc setup'")
	case inst != "" && inst != latest:
		ui.Warn("engine installed : %s  (latest %s - 'winc update' offers a refresh)", inst, latest)
	case inst != "":
		ui.Info("engine installed : %s  (up to date)", inst)
	default:
		ui.Info("engine installed : (version unknown)")
	}
	ui.Info("model catalog   : %d models (%s)", len(catalog.Load(nil).Models), catalog.Source())
	if cfg := loadConfig(); !modelResolvable(cfg, catalog.Load(cfg.CustomModels), cfg.General.DefaultModel) {
		ui.Warn("config: default_model %q is unavailable - 'winc update' will repair it", cfg.General.DefaultModel)
	}
	if clone {
		switch n := <-behind; {
		case n > 0:
			ui.Warn("source is %d commit(s) behind origin - 'winc update' pulls all files + rebuilds", n)
		case n == 0:
			ui.Info("source   : up to date with origin")
		}
		ui.Info("clone    : 'winc update' pulls all repo files + rebuilds winc; offers an engine refresh if it's behind")
	} else {
		ui.Info("prebuilt : 'winc update' refreshes the catalog + offers an engine refresh; redownload the release for code changes")
	}
	ui.Say("")
	return 0
}

func cmdUpdate() int {
	hw := platform.DetectHardware()
	dir := paths.InstallDir()

	if isGitClone(dir) {
		// Clone: pull every repo file, then ALWAYS rebuild so the binary matches the
		// (now-current) source -- not only when the pull moved HEAD. This guarantees a
		// stale binary (e.g. an earlier build that couldn't overwrite a running winc)
		// is brought current. The rebuilt embedded catalogue is then authoritative.
		ui.Info("updating winc source from repo (git pull - all files)...")
		_ = execInherit("git", "-C", dir, "pull", "--ff-only").Run()
		rebuildFromSource()
	} else {
		// Prebuilt: the catalogue cache is how new models arrive without a rebuild.
		ui.Info("refreshing model catalog...")
		have := len(catalog.Load(nil).Models)
		if total, err := catalog.Update(); err != nil {
			ui.Warn("catalog refresh skipped: %v (keeping current %d models)", err, have)
		} else if total != have {
			ui.Good("catalog updated: %d models (was %d)", total, have)
		} else {
			ui.Good("catalog up to date (%d models)", total)
		}
		ui.Info("prebuilt install - redownload the release binary for winc code changes")
	}

	reconcileConfig(hw)
	refreshEngine(hw)
	ui.Good("update complete.")
	return 0
}

// reconcileConfig brings winc.toml forward after an update: it repoints an unresolvable
// default_model to the hardware-recommended model (so e.g. team mode still auto-engages on
// a big model) and appends any config sections new in this version. Non-destructive -- it
// only repairs a broken model reference and APPENDS missing sections; existing user edits
// are left intact.
func reconcileConfig(hw platform.Hardware) {
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)

	if !modelResolvable(cfg, cat, cfg.General.DefaultModel) {
		if rec := recommendModel(cat, hw.MemoryBudgetMB()); rec != nil {
			if err := config.UpdateDefaultModel(rec.Alias); err != nil {
				ui.Warn("config: couldn't repair default_model: %v", err)
			} else {
				ui.Good("config: default_model %q is unavailable -> %q (recommended for this machine)", cfg.General.DefaultModel, rec.Alias)
			}
		}
	}

	if added, err := config.SyncMissingSections(); err != nil {
		ui.Warn("config: couldn't add new sections: %v", err)
	} else if len(added) > 0 {
		ui.Good("config: added new winc.toml section(s): [%s]", strings.Join(added, "], ["))
	}
}

// modelResolvable reports whether a model query maps to something winc can actually run --
// a catalogue alias (downloadable) or an already-downloaded file. A stale alias from an old
// catalogue (e.g. qwen2.5-coder-7b, since removed) is neither.
func modelResolvable(cfg *config.Config, cat *catalog.Catalog, q string) bool {
	if strings.TrimSpace(q) == "" {
		return false
	}
	if cat.Find(q) != nil {
		return true
	}
	p, _ := downloadedPath(cfg, cat, q)
	return p != ""
}

// refreshEngine updates the llama.cpp engine -- but only when it's actually behind,
// and only after confirming (the prebuilt is a large download). Reports when it's
// already current, and installs without prompting when nothing is installed yet.
func refreshEngine(hw platform.Hardware) {
	latest := engine.LatestLlamaTag()
	switch installed := engine.InstalledLlamaTag(); {
	case installed == "":
		ui.Info("installing llama.cpp engine (%s)...", latest)
	case installed == latest:
		ui.Good("engine up to date (%s)", installed)
		return
	default:
		if !ui.Confirm(fmt.Sprintf("Refresh llama.cpp engine %s -> %s? (large download)", installed, latest), false) {
			ui.Say("  keeping engine %s", installed)
			return
		}
	}
	ui.Info("refreshing engine binaries...")
	engine.ClearBinEngine()
	if _, err := engine.AcquireLlama(hw); err != nil {
		ui.Err("engine update failed: %v", err)
		return
	}
	_, _ = engine.AcquireSwap(hw)
	if v := engine.InstalledLlamaTag(); v != "" {
		ui.Good("engine updated to %s", v)
	} else {
		ui.Good("engine refreshed")
	}
}

func isGitClone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func gitHead(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitBehindCount returns how many commits behind its upstream the clone is, or -1 if
// that can't be determined (no upstream / offline). Does a quiet fetch first.
func gitBehindCount(dir string) int {
	_ = exec.Command("git", "-C", dir, "fetch", "--quiet").Run()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD..@{u}").Output()
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

// rebuildFromSource recompiles winc from the (just-pulled) source and swaps the new
// binary into place. The running process keeps executing until it exits; the next
// invocation is the new build. Defensive: any failure leaves the old binary intact.
func rebuildFromSource() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if _, err := exec.LookPath("go"); err != nil {
		ui.Warn("Go not found - rebuild manually to apply source changes:")
		ui.Say("    cd %s && go build -o %s ./cmd/winc", paths.InstallDir(), filepath.Base(exe))
		return
	}
	ui.Info("rebuilding winc from updated source...")
	tmp := exe + ".new"
	cmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", tmp, "./cmd/winc")
	cmd.Dir = paths.InstallDir()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		ui.Warn("rebuild failed: %v (run: go build -o %s ./cmd/winc)", err, filepath.Base(exe))
		_ = os.Remove(tmp)
		return
	}
	// Windows can't delete a running .exe, but it CAN rename it aside; Unix replaces in place.
	if runtime.GOOS == "windows" {
		_ = os.Remove(exe + ".old") // clear a prior update's leftover (now unlocked)
		_ = os.Rename(exe, exe+".old")
	}
	if err := os.Rename(tmp, exe); err != nil {
		ui.Warn("built the new binary but couldn't replace the running one: %v", err)
		ui.Say("    move %s -> %s manually", tmp, exe)
		return
	}
	ui.Good("rebuilt winc from source - re-run your command to use the latest version")
}
