// Package cli implements the winc command dispatch and shared helpers.
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

// Run is the CLI entrypoint; returns a process exit code.
func Run(args []string) int {
	platform.EnableVT()
	cmd := ""
	if len(args) > 0 {
		cmd = strings.ToLower(args[0])
	}
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	switch cmd {
	case "ls", "list":
		return cmdLs()
	case "-s", "start":
		return cmdStart(rest)
	case "-d", "download":
		return cmdDownload(rest)
	case "-r", "rm", "remove":
		return cmdRemove(rest)
	case "-c", "check":
		return cmdCheck()
	case "-u", "update":
		return cmdUpdate()
	case "-n", "uninstall":
		return cmdUninstall(rest)
	case "setup":
		return cmdSetup()
	case "serve":
		return cmdServe(rest)
	case "version", "-v", "--version":
		return cmdVersion()
	case "", "help", "-h", "--help":
		usage()
		return 0
	default:
		ui.Err("unknown command %q", cmd)
		usage()
		return 1
	}
}

func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		ui.Err("config: %v", err)
		os.Exit(1)
	}
	return cfg
}

func modelsDir(cfg *config.Config) string { return paths.ModelsDir(cfg.Paths.ModelsDir) }

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func execInherit(bin string, args ...string) *exec.Cmd {
	c := exec.Command(bin, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c
}

// downloadedPath resolves a query to a downloaded .gguf path. Returns ("", alias)
// when the model is in the catalogue but not downloaded, ("","") when unknown.
func downloadedPath(cfg *config.Config, cat *catalog.Catalog, query string) (path, alias string) {
	md := modelsDir(cfg)
	if m := cat.Find(query); m != nil {
		p := filepath.Join(md, m.File)
		if fileExists(p) {
			return p, m.Alias
		}
		return "", m.Alias
	}
	entries, _ := os.ReadDir(md)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.EqualFold(n, query) || strings.EqualFold(strings.TrimSuffix(n, ".gguf"), query) {
			return filepath.Join(md, n), n
		}
	}
	q := strings.ToLower(query)
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".gguf") && q != "" && strings.Contains(n, q) {
			return filepath.Join(md, e.Name()), e.Name()
		}
	}
	return "", ""
}
