package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

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

func cmdStart(args []string) int {
	cfg := loadConfig()
	cat := catalog.Load(cfg.CustomModels)

	var multi bool
	var team bool
	var noteam bool
	var reasoning string
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--multi":
			multi = true
		case a == "--team":
			team = true
		case a == "--noteam":
			noteam = true
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

	app := cfg.General.DefaultApp
	model := cfg.General.DefaultModel
	if len(pos) >= 1 {
		app = strings.ToLower(pos[0])
	}
	if len(pos) >= 2 {
		model = pos[1]
	}
	if reasoning != "" {
		cfg.Reasoning.Mode = reasoning
	}
	switch app {
	case "claude", "opencode", "openclaw", "cli":
	default:
		ui.Err("unknown app %q (use claude, opencode, openclaw, or cli)", app)
		return 1
	}

	hw := platform.DetectHardware()

	if app == "cli" {
		modelPath, alias := downloadedPath(cfg, cat, model)
		if modelPath == "" {
			reportMissingModel(alias, model)
			return 1
		}
		return runCliChat(cfg, hw, modelPath)
	}

	if wantTeam(app, team, noteam, cfg, cat, model) {
		return startTeam(cfg, cat, hw, app, model)
	}
	if multi {
		return startMulti(cfg, cat, hw, app)
	}

	modelPath, alias := downloadedPath(cfg, cat, model)
	if modelPath == "" {
		reportMissingModel(alias, model)
		return 1
	}
	autoPairDraft(cfg, cat, model) // dense model + downloaded draft -> speculative decoding

	// Small/nano models need loop-safe sampling to call tools reliably (tiny models
	// repeat and emit bad tool-call JSON under default sampling). Prepend so the user's
	// own extra_server_args still win.
	if m := cat.Find(alias); m != nil && (m.Tier == "nano" || m.Tier == "small") {
		if s := engine.SmallModelSamplingArgs(modelPath); len(s) > 0 {
			cfg.Performance.ExtraServerArgs = append(s, cfg.Performance.ExtraServerArgs...)
		}
	}

	if _, err := config.EnsureClaudeLocal(); err != nil {
		ui.Warn("could not create .claude-local: %v", err)
	}

	port := cfg.General.Port
	serverURL := fmt.Sprintf("http://%s:%d", cfg.General.Host, port)
	logPath := filepath.Join(paths.InstallDir(), "llama-server.log")

	ui.Good("Starting %s on %s (sandboxed local instance)", app, filepath.Base(modelPath))
	ui.Info("reasoning: %s", cfg.Reasoning.Mode)
	proc, loadedCtx := startLlamaFitting(cfg, hw, modelPath, port, serverURL, logPath)
	if proc == nil {
		ui.Err("could not start the engine; see %s", logPath)
		return 1
	}
	defer proc.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; proc.Stop(); os.Exit(130) }()

	maxOut := engine.ResolveMaxOutput(cfg, loadedCtx)
	ui.Good("server ready at %s (context %d, max output %d)", serverURL, loadedCtx, maxOut)
	if loadedCtx < 49152 {
		ui.Warn("context is small (%d tokens) - Claude Code may compact often.", loadedCtx)
		if engine.IsMoEFile(modelPath) && !engine.WillOffloadExperts(cfg, hw, modelPath) {
			ui.Say("  tip: set cpu_moe = \"on\" in winc.toml to offload experts to RAM and free VRAM for a much larger context.")
		}
	}

	// Adaptive reasoning: front the server with the in-process router.
	baseURL := serverURL
	if cfg.Reasoning.Mode == "adaptive" {
		r, rerr := router.Start(cfg, serverURL)
		if rerr != nil {
			ui.Warn("router failed (%v); using direct serving", rerr)
		} else {
			defer r.Stop()
			baseURL = r.BaseURL()
			ui.Info("adaptive reasoning router: %s -> %s", baseURL, serverURL)
		}
	}

	if !agent.Available(app) {
		ui.Warn("%s not found on PATH - install it, then re-run.", app)
	}
	slots := agent.Slots{Sonnet: alias, Opus: alias, Haiku: alias}
	env := agent.Env(baseURL, slots, maxOut, loadedCtx, "", "")
	ui.Good("launching %s ... (Ctrl-C to stop)", app)
	if err := agent.Launch(app, env); err != nil {
		ui.Warn("agent exited: %v", err)
	}
	return 0
}

func reportMissingModel(alias, query string) {
	if alias != "" {
		ui.Err("'%s' is not downloaded yet. Run:  winc -d %s", alias, alias)
	} else {
		ui.Err("no downloaded model matches %q. See 'winc ls'.", query)
	}
}

func runCliChat(cfg *config.Config, hw platform.Hardware, modelPath string) int {
	cliBin := engine.LlamaCliPath()
	if cliBin == "" {
		ui.Err("llama-cli not found. Run 'winc setup' to install the engine.")
		return 1
	}
	ngl := engine.GpuLayers(cfg, hw)
	ui.Good("raw chat on %s (Ctrl-C to exit)", filepath.Base(modelPath))
	c := execInherit(cliBin, "-m", modelPath, "-ngl", strconv.Itoa(ngl), "-c", "4096", "-cnv")
	c.Env = server.EnvWithLibPath(filepath.Dir(cliBin))
	if err := c.Run(); err != nil {
		ui.Warn("llama-cli exited: %v", err)
	}
	return 0
}
