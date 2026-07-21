package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"winc/internal/catalog"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/ui"
)

func cmdDownload(args []string) int {
	autoYes := false
	var pos []string
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			autoYes = true
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		ui.Err("usage: winc -d <alias> [-y]   or   winc -d <repo> <file>")
		return 1
	}
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)
	md := modelsDir(cfg)

	var repo, file string
	var m *catalog.Model // nil for a raw <repo> <file> download
	if len(pos) >= 2 && strings.Contains(pos[0], "/") {
		repo, file = pos[0], pos[1]
	} else {
		m = cat.Find(pos[0])
		if m == nil {
			ui.Err("unknown model %q. Run 'winc ls' for aliases, or pass '<repo> <file>'.", pos[0])
			return 1
		}
		repo, file = m.Repo, m.File
	}

	localName := filepath.Base(file)
	if m != nil {
		localName = m.LocalFile()
	}
	target := filepath.Join(md, localName)
	if fileExists(target) {
		ui.Good("already downloaded: %s", localName)
		offerMTPHead(cfg, m, autoYes)
		mtpTip(cat, m)
		return 0
	}
	ui.Good("Downloading %s", localName)
	ui.Say("  from %s", repo)
	if _, err := download.HFDownloadAs(repo, file, md, localName, cfg.HuggingFace.Token); err != nil {
		ui.Err("download failed: %v", err)
		ui.Say("  for gated models set HF_TOKEN, or [huggingface].token in winc.toml")
		return 1
	}
	ui.Good("done: %s", localName)
	if engine.IsMTPFile(localName) {
		ui.Good("MTP variant - winc turns on --spec-type draft-mtp automatically at launch")
	}
	offerMTPHead(cfg, m, autoYes)
	mtpTip(cat, m)
	return 0
}

func cmdRemove(args []string) int {
	yes := false
	var q string
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			yes = true
		default:
			if q == "" {
				q = a
			}
		}
	}
	if q == "" {
		ui.Err("usage: winc -r <alias|filename> [-y]")
		return 1
	}
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)
	path, alias := downloadedPath(cfg, cat, q)
	if path == "" {
		if alias != "" {
			ui.Err("'%s' is not downloaded - nothing to remove.", alias)
		} else {
			ui.Err("no downloaded model matches %q. See 'winc ls'.", q)
		}
		return 1
	}
	gb := 0.0
	if info, err := os.Stat(path); err == nil {
		gb = float64(info.Size()) / 1e9
	}
	if !yes && !ui.Confirm(fmt.Sprintf("Delete %s (%.1f GB)?", filepath.Base(path), gb), false) {
		ui.Say("cancelled - nothing removed.")
		return 0
	}
	if err := os.Remove(path); err != nil {
		ui.Err("remove failed: %v", err)
		return 1
	}
	ui.Good("removed: %s (%.1f GB freed)", filepath.Base(path), gb)
	return 0
}
