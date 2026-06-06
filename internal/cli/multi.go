package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"winc/internal/agent"
	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/router"
	"winc/internal/server"
	"winc/internal/ui"
)

// downloadedModels maps each downloaded gguf to a model key (catalogue alias when
// known, else filename without extension) -> absolute path.
func downloadedModels(cfg *config.Config, cat *catalog.Catalog) map[string]string {
	md := modelsDir(cfg)
	out := map[string]string{}
	entries, _ := os.ReadDir(md)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if m := cat.Find(e.Name()); m != nil {
			key = m.Alias
		}
		out[key] = filepath.Join(md, e.Name())
	}
	return out
}

func resolveSlots(cfg *config.Config, models map[string]string) agent.Slots {
	has := func(k string) bool { _, ok := models[k]; return ok }
	def := ""
	for k := range models {
		def = k
		break
	}
	pick := func(want string) string {
		if want != "" && has(want) {
			return want
		}
		return def
	}
	s := agent.Slots{Sonnet: pick(cfg.Multi.Sonnet), Opus: pick(cfg.Multi.Opus), Haiku: pick(cfg.Multi.Haiku)}
	return s
}

// startMulti runs llama-swap (multi-model). app=="" serves only (waits for
// Ctrl-C); otherwise launches the agent and stops everything on exit.
func startMulti(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware, app string) int {
	models := downloadedModels(cfg, cat)
	if len(models) == 0 {
		ui.Err("no downloaded models. Run 'winc -d <alias>' first.")
		return 1
	}
	serverBin := engine.LlamaServerPath()
	if serverBin == "" {
		ui.Err("llama-server not found. Run 'winc setup'.")
		return 1
	}
	swapBin, err := engine.AcquireSwap(hw)
	if err != nil {
		ui.Err("llama-swap unavailable: %v", err)
		return 1
	}
	yamlPath, err := engine.GenerateSwapYAML(cfg, hw, serverBin, models, cfg.Multi.TTLSeconds)
	if err != nil {
		ui.Err("could not write llama-swap.yaml: %v", err)
		return 1
	}
	if _, err := config.EnsureClaudeLocal(); err != nil {
		ui.Warn("could not create .claude-local: %v", err)
	}

	port := cfg.General.Port
	swapURL := fmt.Sprintf("http://%s:%d", cfg.General.Host, port)
	logPath := filepath.Join(paths.InstallDir(), "llama-swap.log")
	ui.Good("multi-model via llama-swap (%d model(s))", len(models))
	ui.Info("config: %s  |  reasoning: %s", yamlPath, cfg.Reasoning.Mode)

	proc, err := server.Start(swapBin, []string{"-config", yamlPath, "-listen", fmt.Sprintf("%s:%d", cfg.General.Host, port)}, logPath)
	if err != nil {
		ui.Err("failed to start llama-swap: %v", err)
		return 1
	}
	defer proc.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; proc.Stop(); os.Exit(130) }()

	if !server.WaitReady(swapURL, 60*time.Second, proc.Dead) {
		ui.Err("llama-swap did not become ready; see %s", logPath)
		return 1
	}

	baseURL := swapURL
	if cfg.Reasoning.Mode == "adaptive" {
		if r, rerr := router.Start(cfg, swapURL); rerr == nil {
			defer r.Stop()
			baseURL = r.BaseURL()
		} else {
			ui.Warn("router failed (%v); serving direct", rerr)
		}
	}

	if app == "" {
		ui.Good("ready -> set ANTHROPIC_BASE_URL=%s", baseURL)
		ui.Info("llama-swap: %s   (Ctrl-C to stop)", swapURL)
		<-sig
		ui.Say("stopping...")
		return 0
	}

	slots := resolveSlots(cfg, models)
	ui.Good("model slots: sonnet=%s  opus=%s  haiku=%s", slots.Sonnet, slots.Opus, slots.Haiku)
	if !agent.Available(app) {
		ui.Warn("%s not found on PATH - install it, then re-run.", app)
	}
	maxOut := engine.ResolveMaxOutput(cfg, engine.ResolveContext(cfg, hw, engine.FileMB(models[slots.Sonnet])))
	env := agent.Env(baseURL, slots, maxOut)
	ui.Good("launching %s ... (Ctrl-C to stop)", app)
	if err := agent.Launch(app, env); err != nil {
		ui.Warn("agent exited: %v", err)
	}
	return 0
}
