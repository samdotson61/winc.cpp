package cli

import (
	"os"
	"path/filepath"

	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

func dirSize(p string) int64 {
	var total int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

func pathSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if fi.IsDir() {
		return dirSize(p)
	}
	return fi.Size()
}

// cmdUninstall removes installed components (engine, models, generated files) and
// the PATH entry, keeping winc.toml, the winc binary, and any source.
func cmdUninstall(args []string) int {
	yes := false
	for _, a := range args {
		if a == "-y" || a == "--yes" {
			yes = true
		}
	}
	cfg := loadConfig()
	dir := paths.InstallDir()
	targets := []string{
		paths.BinDir(),
		modelsDir(cfg),
		paths.ClaudeLocalDir(),
		paths.LlamaDir(),
		paths.LlamaSwapYAML(),
		filepath.Join(dir, "llama-server.log"),
		filepath.Join(dir, ".winc-dl"),
		filepath.Join(dir, ".winc-timings.json"),
	}
	type item struct {
		path string
		size int64
	}
	var present []item
	var total int64
	for _, p := range targets {
		if _, err := os.Stat(p); err == nil {
			s := pathSize(p)
			total += s
			present = append(present, item{p, s})
		}
	}
	onPath := platform.OnPath(dir)
	if len(present) == 0 && !onPath {
		ui.Good("nothing to uninstall in %s", dir)
		return 0
	}

	ui.Say("")
	ui.Say("This will remove from %s:", dir)
	for _, it := range present {
		ui.Say("  %-26s %8.2f GB", filepath.Base(it.path), float64(it.size)/1e9)
	}
	if onPath {
		ui.Say("  (also removes winc from your user PATH)")
	}
	ui.Say("  total: %.2f GB", float64(total)/1e9)
	ui.Say("  keeps: winc.toml, winc%s, and any source", platform.ExeSuffix())
	ui.Say("")
	if !yes && !ui.Confirm("Uninstall winc components?", false) {
		ui.Say("cancelled - nothing removed.")
		return 0
	}
	for _, it := range present {
		if err := os.RemoveAll(it.path); err != nil {
			ui.Warn("could not remove %s: %v", filepath.Base(it.path), err)
		} else {
			ui.Good("removed %s", filepath.Base(it.path))
		}
	}
	if onPath {
		if err := platform.RemoveFromPath(dir); err != nil {
			ui.Warn("could not update PATH: %v", err)
		} else {
			ui.Good("removed winc from user PATH (open a new terminal)")
		}
	}
	ui.Say("")
	ui.Good("uninstall complete. To remove everything, delete %s", dir)
	return 0
}
