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

	if wantTeam(app, team, noteam, cfg, cat, hw, model) {
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
	rememberLastUsed(cfg, app, alias)
	autoPairDraft(cfg, cat, model) // dense model + downloaded draft -> speculative decoding
	// (family-correct sampling is applied for all tiers inside engine.ServerArgs)

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
	// Provision the agent-side notes: the REAL window (the agent's own UI cannot
	// show local windows below 100k), measured speeds, and small-window practices.
	// Single mode runs llama's auto-parallel with a UNIFIED KV pool, so every
	// request can use the full window (verified on the shipped engine).
	if err := config.WriteAgentNotes(loadedCtx, loadedCtx, lastBench.gen, lastBench.pp); err != nil {
		ui.Warn("could not write agent notes: %v", err)
	}
	if loadedCtx < 49152 {
		ui.Warn("context is small (%d tokens) - Claude Code may compact often.", loadedCtx)
		if engine.IsMoEFile(modelPath) && !engine.WillOffloadExperts(cfg, hw, modelPath) {
			ui.Say("  tip: set cpu_moe = \"on\" in winc.toml to offload experts to RAM and free VRAM for a much larger context.")
		}
	}

	// Always front the server with the in-process router: in adaptive mode it injects the
	// thinking budget, and in EVERY mode it rewrites llama-server's context-overflow error
	// into the wording Claude Code recognizes (so a big tool_result doesn't surface as
	// "<model> is temporarily unavailable" and block the command in auto mode).
	baseURL := serverURL
	r, rerr := router.Start(cfg, serverURL, loadedCtx)
	if rerr != nil {
		ui.Warn("router failed (%v); using direct serving", rerr)
	} else {
		defer r.Stop()
		baseURL = r.BaseURL()
		if cfg.Reasoning.Mode == "adaptive" {
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
	// Surface how often the session hit the context wall (each one was rewritten into
	// Claude Code's compaction signal). A recurring count means the context is too
	// small for the workload -- the actionable follow-up lives in the warning above.
	if n := r.Stats().Overflows; n > 0 {
		ui.Info("session: hit the context limit %d time(s) - each was rewritten so the agent compacts instead of stalling", n)
	}
	return 0
}

// rememberLastUsed persists a successful start's agent + model as the new
// defaults, so a bare `winc -s` brings back the last used model with the last
// used agent. Best-effort and quiet: a write failure never blocks the launch,
// nothing is written when nothing changed, and it only runs AFTER the model
// resolved to a downloaded file -- a typo can never become the default. The
// `cli` chat utility is excluded so a quick test chat doesn't flip the defaults.
func rememberLastUsed(cfg *config.Config, app, alias string) {
	if app == "" || app == "cli" || alias == "" {
		return
	}
	if !strings.EqualFold(alias, cfg.General.DefaultModel) {
		if err := config.UpdateDefaultModel(alias); err == nil {
			ui.Dim("default model -> %s (a bare 'winc -s' now starts it)", alias)
		}
	}
	if !strings.EqualFold(app, cfg.General.DefaultApp) {
		if err := config.UpdateDefaultApp(app); err == nil {
			ui.Dim("default app -> %s", app)
		}
	}
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
