package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"winc/internal/catalog"
	"winc/internal/ui"
)

func cmdLs() int {
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)
	md := modelsDir(cfg)

	ui.Say("")
	ui.Say("Downloaded (in %s):", md)
	var files []os.DirEntry
	if entries, err := os.ReadDir(md); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
				files = append(files, e)
			}
		}
	}
	if len(files) == 0 {
		ui.Say("  (none yet - use 'winc -d <alias>')")
	} else {
		sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
		for _, f := range files {
			gb := 0.0
			if info, err := f.Info(); err == nil {
				gb = float64(info.Size()) / 1e9
			}
			ui.Say("  %-46s %6.1f GB", f.Name(), gb)
		}
	}

	ui.Say("")
	ui.Say("Available to download (alias  ~size  model):")
	for _, tier := range catalog.TierOrder {
		models := cat.ByTier(tier)
		if len(models) == 0 {
			continue
		}
		ui.Say("")
		ui.Say("  -- %s  (%s) --", tier, cat.Tiers[tier])
		for _, m := range models {
			mark := ""
			if fileExists(filepath.Join(md, m.LocalFile())) {
				mark = "  [installed]"
			}
			ui.Say("  %-20s %9s  %s%s", m.Alias, m.Size, m.Name, mark)
		}
	}
	ui.Say("")
	ui.Say("Download:  winc -d <alias>      Start:  winc -s claude <alias>")
	ui.Say("")
	return 0
}
