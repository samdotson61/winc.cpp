package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"winc/internal/agent"
	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/router"
	"winc/internal/server"
	"winc/internal/ui"
)

// startTeam runs the heterogeneous agent hierarchy: the launched model orchestrates
// as the main agent on the GPU, and small worker models run on the CPU mapped onto
// Claude Code's sonnet (collator/review) and haiku (research fan-out + Explore) tiers.
// A model-aware router fans each tier's requests to the right backend. Workers stay on
// the CPU so they never eat the main model's VRAM or shrink its context.
func startTeam(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware, app, mainQuery string) int {
	mainPath, mainAlias := downloadedPath(cfg, cat, mainQuery)
	if mainPath == "" {
		reportMissingModel(mainAlias, mainQuery)
		return 1
	}
	autoPairDraft(cfg, cat, mainQuery) // the main model still gets its draft / MTP speedup

	if _, err := config.EnsureClaudeLocal(); err != nil {
		ui.Warn("could not create .claude-local: %v", err)
	}
	if err := config.WriteTeamAgents(); err != nil {
		ui.Warn("could not write team agents: %v", err)
	}

	// Which worker(s) to run depends on the subagents policy: "haiku"/"sonnet" force ALL
	// subagents (Task tool + Workflow fan-out) onto that one worker; "tiered" runs both
	// and relies on per-agent pins. Only ensure/download the workers we'll actually run.
	sub := strings.ToLower(strings.TrimSpace(cfg.Team.Subagents))
	if sub != "sonnet" && sub != "tiered" {
		sub = "haiku"
	}
	runSonnet := sub == "sonnet" || sub == "tiered"
	runHaiku := sub == "haiku" || sub == "tiered"

	var sonnetPath, sonnetAlias, haikuPath, haikuAlias string
	if runSonnet {
		sonnetPath, sonnetAlias = ensureWorker(cfg, cat, firstNonEmpty(cfg.Team.Sonnet, "qwen3.5-4b"), "sonnet (collator / code-review)")
	}
	if runHaiku {
		haikuPath, haikuAlias = ensureWorker(cfg, cat, firstNonEmpty(cfg.Team.Haiku, "qwen3.5-0.8b"), "haiku (research fan-out)")
	}

	var procs []*server.Proc
	stopAll := func() {
		for i := len(procs) - 1; i >= 0; i-- {
			procs[i].Stop()
		}
	}
	defer stopAll()

	// Main model on the primary port (GPU, full fitting ladder), as in single mode.
	port := cfg.General.Port
	mainURL := fmt.Sprintf("http://%s:%d", cfg.General.Host, port)
	logDir := paths.InstallDir()
	ui.Good("team: main %s on %s", mainAlias, gpuOrCPU(cfg, hw))
	ui.Info("reasoning: %s", cfg.Reasoning.Mode)
	mainProc, loadedCtx := startLlamaFitting(cfg, hw, mainPath, port, mainURL, filepath.Join(logDir, "llama-server.log"))
	if mainProc == nil {
		ui.Err("could not start the main engine; see %s", filepath.Join(logDir, "llama-server.log"))
		return 1
	}
	procs = append(procs, mainProc)

	serverBin := engine.LlamaServerPath()
	par := cfg.Team.Parallel
	if par <= 0 {
		par = 4
	}

	// Workers on the CPU, each on its own port, routed by model name. A worker that's
	// missing or fails to start simply falls back to the main model.
	slots := agent.Slots{Opus: mainAlias, Sonnet: mainAlias, Haiku: mainAlias}
	var routes []router.Route
	if runSonnet {
		if p, url, alias := startWorker(cfg, hw, serverBin, sonnetPath, sonnetAlias, mainAlias, 2, 32768, "sonnet", filepath.Join(logDir, "worker-sonnet.log")); p != nil {
			procs = append(procs, p)
			routes = append(routes, router.Route{Model: alias, Upstream: url, Think: ""}) // collator/review: adaptive
			slots.Sonnet = alias
		}
	}
	if runHaiku {
		if p, url, alias := startWorker(cfg, hw, serverBin, haikuPath, haikuAlias, mainAlias, par, par*8192, "haiku", filepath.Join(logDir, "worker-haiku.log")); p != nil {
			procs = append(procs, p)
			routes = append(routes, router.Route{Model: alias, Upstream: url, Think: "low"}) // research fan-out: brief thinking = reliable tool use, still fast
			slots.Haiku = alias
		}
	}

	// Force every subagent (Task + Workflow fan-out) onto the chosen worker, so a
	// deep-research fan-out uses it instead of cloning the big model. "tiered" leaves it
	// unset and relies on per-agent pins (research->haiku, collator/review->sonnet).
	subagentModel := ""
	switch sub {
	case "haiku":
		subagentModel = slots.Haiku
	case "sonnet":
		subagentModel = slots.Sonnet
	}

	// The model-aware router is mandatory in team mode -- it is what dispatches each
	// tier to its backend (and still does adaptive reasoning for the main/sonnet tiers).
	rtr, err := router.StartTeam(cfg, routes, mainURL)
	if err != nil {
		ui.Err("team router failed: %v", err)
		return 1
	}
	defer rtr.Stop()
	baseURL := rtr.BaseURL()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; stopAll(); os.Exit(130) }()

	maxOut := engine.ResolveMaxOutput(cfg, loadedCtx)
	switch {
	case sub == "tiered":
		ui.Good("team ready  main=%s  sonnet=%s  haiku=%s", slots.Opus, slots.Sonnet, slots.Haiku)
		ui.Info("hierarchy: main orchestrates | sonnet=collator/review | haiku=research fan-out + Explore")
		if slots.Sonnet == mainAlias && slots.Haiku == mainAlias {
			ui.Warn("no workers running - every tier falls back to the main model")
		}
	case subagentModel != "" && subagentModel != mainAlias:
		ui.Good("team ready  main=%s  all subagents -> %s", mainAlias, subagentModel)
		ui.Info("every subagent + research fan-out (Task & Workflow) runs on the small worker; main orchestrates")
	default:
		ui.Warn("team: the worker didn't start - subagents fall back to the main model (see the worker log)")
	}
	if !agent.Available(app) {
		ui.Warn("%s not found on PATH - install it, then re-run.", app)
	}
	env := agent.Env(baseURL, slots, maxOut, loadedCtx, mainAlias, subagentModel) // pin main + force subagents onto the worker
	ui.Good("launching %s ... (Ctrl-C to stop)", app)
	if err := agent.Launch(app, env); err != nil {
		ui.Warn("agent exited: %v", err)
	}
	return 0
}

// ensureWorker resolves a worker model to a local path, offering to download it when
// missing (turnkey). Returns ("", alias) when unavailable so its tier degrades to the
// main model instead of failing the launch.
func ensureWorker(cfg *config.Config, cat *catalog.Catalog, query, role string) (path, alias string) {
	if p, a := downloadedPath(cfg, cat, query); p != "" {
		return p, a
	}
	m := cat.Find(query)
	if m == nil {
		ui.Warn("team: %s worker %q isn't in the catalog - that tier will use the main model", role, query)
		return "", query
	}
	if !ui.Confirm(fmt.Sprintf("team: download the %s worker %s (%s, %s)?", role, m.Alias, m.Size, m.Tier), true) {
		ui.Dim("skipped %s - the %s tier will fall back to the main model", m.Alias, role)
		return "", m.Alias
	}
	md := modelsDir(cfg)
	ui.Good("Downloading worker %s", m.LocalFile())
	ui.Say("  from %s", m.Repo)
	if _, err := download.HFDownloadAs(m.Repo, m.File, md, m.LocalFile(), cfg.HuggingFace.Token); err != nil {
		ui.Warn("worker download failed: %v - the %s tier falls back to the main model", err, role)
		return "", m.Alias
	}
	return filepath.Join(md, m.LocalFile()), m.Alias
}

// startWorker launches one CPU worker (dense small model) and returns its proc, URL,
// and alias, or (nil,"","") when it has no model, no engine, an alias colliding with
// the main model, or it fails to come up. parallel slots serve concurrent subagents.
func startWorker(cfg *config.Config, hw platform.Hardware, serverBin, modelPath, alias, mainAlias string, parallel, ctx int, role, logPath string) (*server.Proc, string, string) {
	if modelPath == "" || serverBin == "" {
		return nil, "", ""
	}
	if strings.EqualFold(alias, mainAlias) {
		ui.Dim("team: %s worker is the same model as main - skipping (no separate tier needed)", role)
		return nil, "", ""
	}
	pnum := freePort()
	if pnum == 0 {
		ui.Warn("team: no free port for the %s worker - that tier falls back to main", role)
		return nil, "", ""
	}

	// Worker config: force CPU (-ngl 0) so it never touches the main model's VRAM, and
	// drop the main model's draft/MTP/extra flags (they don't apply to the worker). Run
	// the worker server in adaptive reasoning so the router governs thinking per request
	// (a low budget for the research tier -- small models need a little thinking for tools).
	wc := *cfg
	wc.Performance.GpuLayers = "0"
	wc.Performance.DraftModel = ""
	wc.Performance.Mtp = "off"
	wc.Performance.ExtraServerArgs = nil
	wc.Reasoning.Mode = "adaptive"
	wc.General.Port = pnum

	args := engine.ServerArgs(&wc, hw, modelPath, pnum, "", ctx)
	args = append(args, "--parallel", strconv.Itoa(parallel))
	// Loop-safe, family-appropriate sampling: tiny models repeat and emit bad tool-call
	// JSON under default sampling. No-op for families we have no profile for.
	args = append(args, engine.SmallModelSamplingArgs(modelPath)...)
	proc, err := server.Start(serverBin, args, logPath)
	if err != nil {
		ui.Warn("team: %s worker failed to launch: %v - tier falls back to main", role, err)
		return nil, "", ""
	}
	url := fmt.Sprintf("http://%s:%d", cfg.General.Host, pnum)
	ui.Info("team: %s %s on CPU (port %d, %d slots)", role, alias, pnum, parallel)
	if !server.WaitReady(url, "/health", 180*time.Second, proc.Dead) {
		ui.Warn("team: %s worker didn't become ready - tier falls back to main; see %s", role, logPath)
		proc.Stop()
		return nil, "", ""
	}
	return proc, url, alias
}

// freePort grabs an ephemeral localhost port for a worker server (0 on failure).
func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func gpuOrCPU(cfg *config.Config, hw platform.Hardware) string {
	if engine.GpuLayers(cfg, hw) > 0 {
		return "GPU"
	}
	return "CPU"
}

// wantTeam decides whether to run team mode. Explicit flags win (--noteam off, --team on);
// otherwise [team].mode governs: "off" never, "on" always, "auto" (default) engages for a
// big-enough main model. Team's tier env is Claude Code-specific, so other apps stay single.
func wantTeam(app string, teamFlag, noteamFlag bool, cfg *config.Config, cat *catalog.Catalog, model string) bool {
	if noteamFlag {
		return false
	}
	if teamFlag {
		return true
	}
	if app != "claude" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Team.Mode)) {
	case "off":
		return false
	case "on":
		return true
	default: // auto
		return mainBigEnoughForTeam(cfg, cat, model)
	}
}

// mainBigEnoughForTeam reports whether the main model is large enough that offloading
// subagents to a small worker is worthwhile -- mid tier or larger, or (for a model not in
// the catalog) a file of at least ~8 GB. Nano/small main models just run single.
func mainBigEnoughForTeam(cfg *config.Config, cat *catalog.Catalog, model string) bool {
	if m := cat.Find(model); m != nil {
		switch m.Tier {
		case "mid", "large", "xl":
			return true
		default:
			return false
		}
	}
	if p, _ := downloadedPath(cfg, cat, model); p != "" {
		return engine.FileMB(p) >= 8000
	}
	return false
}
