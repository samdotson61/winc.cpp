package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"winc/internal/catalog"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/router"
	"winc/internal/server"
	"winc/internal/ui"
)

// cmdServe runs the local server (+ adaptive router) with no agent attached -
// handy for pointing your own client at it, or for testing. Ctrl-C to stop.
func cmdServe(args []string) int {
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)

	var reasoning string
	var multi bool
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--multi":
			multi = true
		case a == "--reasoning":
			if i+1 < len(args) {
				reasoning = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--reasoning="):
			reasoning = strings.TrimPrefix(a, "--reasoning=")
		default:
			pos = append(pos, a)
		}
	}
	model := cfg.General.DefaultModel
	if len(pos) >= 1 {
		model = pos[0]
	}
	if reasoning != "" {
		cfg.Reasoning.Mode = reasoning
	}

	if multi {
		return startMulti(cfg, cat, platform.DetectHardware(), "")
	}

	modelPath, alias := downloadedPath(cfg, cat, model)
	if modelPath == "" {
		reportMissingModel(alias, model)
		return 1
	}
	hw := platform.DetectHardware()
	serverBin := engine.LlamaServerPath()
	if serverBin == "" {
		ui.Err("llama-server not found. Run 'winc setup' to install the engine.")
		return 1
	}

	port := cfg.General.Port
	serverURL := fmt.Sprintf("http://%s:%d", cfg.General.Host, port)
	logPath := filepath.Join(paths.InstallDir(), "llama-server.log")
	sargs := engine.ServerArgs(cfg, hw, modelPath, port, "")

	ui.Good("serve: %s (%s)", alias, filepath.Base(modelPath))
	ui.Info("engine: %s  |  reasoning: %s", serverBin, cfg.Reasoning.Mode)
	proc, err := server.Start(serverBin, sargs, logPath)
	if err != nil {
		ui.Err("failed to start llama-server: %v", err)
		return 1
	}
	defer proc.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	ui.Info("loading model + waiting for server...")
	if !server.WaitReady(serverURL, 240*time.Second, proc.Dead) {
		ui.Err("server did not become ready; see %s", logPath)
		return 1
	}

	baseURL := serverURL
	if cfg.Reasoning.Mode == "adaptive" {
		if r, rerr := router.Start(cfg, serverURL); rerr == nil {
			defer r.Stop()
			baseURL = r.BaseURL()
		} else {
			ui.Warn("router failed (%v); serving direct", rerr)
		}
	}

	ui.Good("ready -> set ANTHROPIC_BASE_URL=%s", baseURL)
	ui.Info("llama-server: %s   (Ctrl-C to stop)", serverURL)
	<-sig
	ui.Say("stopping...")
	return 0
}
