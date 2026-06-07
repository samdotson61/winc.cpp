package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"winc/internal/catalog"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func cmdCheck() int {
	dir := paths.InstallDir()
	ui.Say("")
	ui.Say("Checking for updates...")
	ui.Info("winc version    : %s", Version)
	if tag := engine.LatestWincTag(); tag != "" && strings.TrimPrefix(tag, "v") != Version {
		ui.Warn("newer winc available: %s (you have %s)", tag, Version)
	}
	ui.Good("llama.cpp latest : %s", engine.LatestLlamaTag())
	ui.Good("llama-swap latest: %s", engine.LatestSwapTag())
	if p := engine.LlamaServerPath(); p != "" {
		ui.Info("engine installed : %s", p)
	} else {
		ui.Warn("engine not installed - run 'winc setup'")
	}
	src := "built-in"
	if _, err := os.Stat(paths.CatalogPath()); err == nil {
		src = "updated cache"
	}
	ui.Info("model catalog   : %d models (%s)", len(catalog.Load(nil).Models), src)
	if isGitClone(dir) {
		switch n := gitBehindCount(dir); {
		case n > 0:
			ui.Warn("source is %d commit(s) behind origin - 'winc update' pulls all files + rebuilds", n)
		case n == 0:
			ui.Info("source   : up to date with origin")
		}
		ui.Info("clone    : 'winc update' pulls ALL repo files, rebuilds winc, refreshes engine + catalog")
	} else {
		ui.Info("prebuilt : 'winc update' refreshes engine + catalog; redownload the release for code changes")
	}
	ui.Say("")
	return 0
}

func cmdUpdate() int {
	hw := platform.DetectHardware()
	dir := paths.InstallDir()

	// 1. Pull all repo files (clone only). Track whether anything actually changed.
	clone, pulled := isGitClone(dir), false
	if clone {
		ui.Info("updating winc source from repo (git pull - all files)...")
		before := gitHead(dir)
		_ = execInherit("git", "-C", dir, "pull", "--ff-only").Run()
		after := gitHead(dir)
		pulled = after != "" && after != before
	}

	// 2. Refresh the model catalog (works for clone + prebuilt).
	ui.Info("refreshing model catalog...")
	have := len(catalog.Load(nil).Models)
	if total, err := catalog.Update(); err != nil {
		ui.Warn("catalog refresh skipped: %v (keeping current %d models)", err, have)
	} else if total != have {
		ui.Good("catalog updated: %d models (was %d)", total, have)
	} else {
		ui.Good("catalog up to date (%d models)", total)
	}

	// 3. Rebuild from the pulled source so ALL repo changes (code, embedded catalog,
	//    fixes) actually take effect -- a git pull alone leaves the old binary running.
	switch {
	case clone && pulled:
		rebuildFromSource()
	case clone:
		ui.Info("source already up to date - no rebuild needed")
	default:
		ui.Info("prebuilt install - redownload the release binary to get winc code changes")
	}

	// 4. Refresh engine binaries.
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
