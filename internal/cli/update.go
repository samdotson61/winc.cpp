package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
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
		selfUpdatePrebuilt()
	}

	reconcileConfig(hw)
	refreshEngine(hw)
	// PATH reconcile: older installs recorded PATH only for bash/zsh -- fish-first
	// distros (CachyOS) never saw it, and a moved folder breaks the recorded entry
	// anyway. If the LIVE environment can't reach winc, re-apply for every
	// supported shell (idempotent).
	if dir := paths.InstallDir(); !liveOnPath(dir) {
		_ = platform.AddToPath(dir)
		ui.Good("added winc to PATH (bash/zsh/profile, fish, ~/.local/bin) - open a NEW terminal to pick it up")
	}
	ui.Good("update complete.")
	return 0
}

// liveOnPath reports whether dir is reachable in THIS process's PATH -- the
// check that matters for "I have to run ./winc from its folder".
func liveOnPath(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}

// wincAssetName is this platform's release asset, exactly as `make release`
// names them (dist/winc-<os>-<arch>[.exe]).
func wincAssetName() string {
	name := "winc-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// selfUpdatePrebuilt replaces a prebuilt winc binary with the latest release
// build for this OS/arch. Previously `winc update` refreshed everything EXCEPT
// winc itself on prebuilt installs -- they stayed stranded on old code (and old
// fixes) forever unless the user manually re-downloaded. The download is
// sha256-verified against the release's published digests (a mismatch is a
// hard fail and the file is discarded); the swap uses the same rename dance as
// rebuildFromSource, so the running process finishes normally and the NEXT
// invocation is the new build.
func selfUpdatePrebuilt() {
	tag := engine.LatestWincTag()
	if tag == "" {
		ui.Warn("could not reach the winc releases API - keeping the current binary")
		return
	}
	if strings.TrimPrefix(tag, "v") == Version {
		ui.Good("winc is up to date (%s)", Version)
		return
	}
	asset := wincAssetName()
	url, digest, ok := engine.WincAsset(asset)
	if !ok {
		ui.Warn("no release asset for this platform (%s) - keeping the current binary", asset)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		ui.Warn("can't locate the running binary: %v", err)
		return
	}
	tmp := exe + ".new"
	ui.Info("updating winc %s -> %s ...", Version, tag)
	if err := download.Fetch(url, tmp, nil, asset); err != nil {
		ui.Warn("download failed: %v - keeping the current binary", err)
		return
	}
	if digest != "" {
		if err := verifySHA256(tmp, digest); err != nil {
			_ = os.Remove(tmp)
			ui.Err("release digest mismatch: %v (download discarded)", err)
			return
		}
	} else {
		ui.Dim("release published no digest for %s - proceeding unverified", asset)
	}
	_ = os.Chmod(tmp, 0o755)
	// Windows can't delete a running .exe, but it CAN rename it aside; Unix replaces in place.
	if runtime.GOOS == "windows" {
		_ = os.Remove(exe + ".old")
		_ = os.Rename(exe, exe+".old")
	}
	if err := os.Rename(tmp, exe); err != nil {
		ui.Warn("downloaded the new binary but couldn't replace the running one: %v", err)
		ui.Say("    move %s -> %s manually", tmp, exe)
		return
	}
	ui.Good("winc updated to %s - re-run your command to use it", tag)
}

// verifySHA256 checks a file against a "sha256:<hex>" digest.
func verifySHA256(path, digest string) error {
	want := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 %s != published %s", got, want)
	}
	return nil
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
