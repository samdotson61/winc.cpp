package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/router"
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
		return startMulti(cfg, cat, platform.DetectHardwareCached(), "")
	}

	modelPath, alias := downloadedPath(cfg, cat, model)
	if modelPath == "" {
		reportMissingModel(alias, model)
		return 1
	}
	autoPairDraft(cfg, cat, model) // dense model + downloaded draft -> speculative decoding
	hw := platform.DetectHardwareCached()
	ensureGPUSpeeds(cfg, cat, &hw) // multi-GPU bandwidth weights, measured once per machine

	port := cfg.General.Port
	serverURL := fmt.Sprintf("http://%s:%d", cfg.General.Host, port)
	logPath := filepath.Join(paths.InstallDir(), "llama-server.log")
	ui.Good("serve: %s (%s)", alias, filepath.Base(modelPath))
	ui.Info("reasoning: %s", cfg.Reasoning.Mode)
	proc, loadedCtx := startLlamaFitting(cfg, hw, modelPath, port, serverURL, logPath)
	if proc == nil {
		ui.Err("could not start the engine; see %s", logPath)
		return 1
	}
	defer proc.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	// Provision the agent-side notes (real window, measured speeds) -- whatever
	// client gets pointed at this server can read them. Single/serve mode runs
	// llama's auto-parallel with a UNIFIED KV pool, so every request can use the
	// full window (verified on the shipped engine).
	if err := config.WriteAgentNotes(loadedCtx, loadedCtx, lastBench.gen, lastBench.pp); err != nil {
		ui.Warn("could not write agent notes: %v", err)
	}

	baseURL := serverURL
	if cfg.Reasoning.Mode == "adaptive" {
		if r, rerr := router.Start(cfg, serverURL, loadedCtx); rerr == nil {
			defer r.Stop()
			baseURL = r.BaseURL()
		} else {
			ui.Warn("router failed (%v); serving direct", rerr)
		}
	}

	ui.Good("ready -> set ANTHROPIC_BASE_URL=%s", baseURL)
	ui.Info("llama-server: %s  (context %d, max output %d)   (Ctrl-C to stop)", serverURL, loadedCtx, engine.ResolveMaxOutput(cfg, loadedCtx))
	<-sig
	ui.Say("stopping...")
	return 0
}
