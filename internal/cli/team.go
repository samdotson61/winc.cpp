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

	// Subagent policy: "dynamic" (default) runs both workers behind a load-based escalation
	// ladder; "haiku"/"sonnet" force one worker; "tiered" runs both with per-agent pins.
	sub := strings.ToLower(strings.TrimSpace(cfg.Team.Subagents))
	switch sub {
	case "haiku", "sonnet", "tiered", "dynamic":
	default:
		sub = "dynamic"
	}
	runSonnet := sub == "sonnet" || sub == "tiered" || sub == "dynamic"
	runHaiku := sub == "haiku" || sub == "tiered" || sub == "dynamic"
	runMid := sub == "dynamic" && midEnabled(cfg.Team.Mid) // optional middle rung (e.g. the 2B)

	// Fit the workers to available RAM, smallest-first: keep as many as fit alongside the
	// main model and drop the largest first. Graceful degradation -- a tight box runs only
	// the worker(s) that fit (down to just the 0.8B), and falls back to a single model only
	// when not even the smallest fits (teamWorthwhile already gated that case out).
	budget := workerRAMBudgetMB(cfg, hw, mainPath, engine.FileMB(mainPath))
	usedRAM := 0
	fitsRAM := func(run bool, alias string) bool {
		if !run {
			return false
		}
		r := workerRAMMB(cat, alias)
		if usedRAM+r <= budget {
			usedRAM += r
			return true
		}
		ui.Dim("team: not enough RAM for the %s worker - skipping it (the ladder just tops out lower)", alias)
		return false
	}
	runHaiku = fitsRAM(runHaiku, firstNonEmpty(cfg.Team.Haiku, "qwen3.5-0.8b"))
	runMid = fitsRAM(runMid, cfg.Team.Mid)
	runSonnet = fitsRAM(runSonnet, firstNonEmpty(cfg.Team.Sonnet, "qwen3.5-4b"))

	var sonnetPath, sonnetAlias, haikuPath, haikuAlias, midPath, midAlias string
	if runSonnet {
		sonnetPath, sonnetAlias = ensureWorker(cfg, cat, firstNonEmpty(cfg.Team.Sonnet, "qwen3.5-4b"), "sonnet (collator / code-review)")
	}
	if runMid {
		midPath, midAlias = ensureWorker(cfg, cat, cfg.Team.Mid, "mid (light research)")
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

	// Dynamic mode may escalate heavy subagents onto the main GPU model -- but only when
	// there's VRAM headroom to serve them concurrently. When so, give the main server a
	// second parallel slot (which halves its per-slot context -> headCtx below).
	mainEscalate := sub == "dynamic" && engine.MainEscalationOK(cfg, hw, mainPath)
	if mainEscalate {
		cfg.Performance.ExtraServerArgs = append([]string{"--parallel", "2"}, cfg.Performance.ExtraServerArgs...)
	}

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
	headCtx := loadedCtx
	if mainEscalate {
		headCtx = loadedCtx / 2 // --parallel 2 splits the window into per-slot contexts
	}

	serverBin := engine.LlamaServerPath()
	par := cfg.Team.Parallel
	if par <= 0 {
		par = 4
	}

	// Launch the worker(s) on the CPU, capturing each one's URL.
	slots := agent.Slots{Opus: mainAlias, Sonnet: mainAlias, Haiku: mainAlias}
	var sonnetURL, midURL, haikuURL string
	if runSonnet {
		if p, url, alias := startWorker(cfg, hw, serverBin, sonnetPath, sonnetAlias, mainAlias, 2, 32768, "sonnet", filepath.Join(logDir, "worker-sonnet.log")); p != nil {
			procs = append(procs, p)
			sonnetURL, slots.Sonnet = url, alias
		}
	}
	if runMid {
		if p, url, _ := startWorker(cfg, hw, serverBin, midPath, midAlias, mainAlias, par, par*8192, "mid", filepath.Join(logDir, "worker-mid.log")); p != nil {
			procs = append(procs, p)
			midURL = url // ladder-only rung; not mapped to a Claude Code tier
		}
	}
	if runHaiku {
		if p, url, alias := startWorker(cfg, hw, serverBin, haikuPath, haikuAlias, mainAlias, par, par*8192, "haiku", filepath.Join(logDir, "worker-haiku.log")); p != nil {
			procs = append(procs, p)
			haikuURL, slots.Haiku = url, alias
		}
	}

	// Build the dispatch: explicit per-agent routes (tiered), or a subagent tag + escalation
	// ladder (dynamic/haiku/sonnet) that forces every subagent (Task + Workflow) onto the
	// worker(s). The HEAD (pinned to the main model) always reaches the main backend.
	const catchAll = 1 << 30
	var routes []router.Route
	var ladder []router.Tier
	var ladderTag, subagentModel string
	// Per-tier tool allowlists: tiny workers (0.8B/2B) stay research-only; the 4B also gets
	// Write (collation/review); the HEAD model keeps every tool (nil = no stripping).
	workerTools := cfg.Team.WorkerTools
	sonnetTools := cfg.Team.SonnetTools
	switch sub {
	case "tiered": // per-agent pins; generic/Workflow agents inherit the main model
		if sonnetURL != "" {
			routes = append(routes, router.Route{Model: slots.Sonnet, Upstream: sonnetURL, Think: "", Tools: sonnetTools})
		}
		if haikuURL != "" {
			routes = append(routes, router.Route{Model: slots.Haiku, Upstream: haikuURL, Think: "low", Tools: workerTools})
		}
	case "sonnet": // force all subagents to the 4B
		subagentModel, ladderTag = sonnetAlias, sonnetAlias
		if sonnetURL != "" {
			ladder = []router.Tier{{Upstream: sonnetURL, Think: "", MaxEstTokens: catchAll, Tools: sonnetTools}}
		}
	case "haiku": // force all subagents to the 0.8B
		subagentModel, ladderTag = haikuAlias, haikuAlias
		if haikuURL != "" {
			ladder = []router.Tier{{Upstream: haikuURL, Think: "low", MaxEstTokens: catchAll, Tools: workerTools}}
		}
	default: // dynamic: tag every subagent with the haiku alias, escalate by request load
		subagentModel, ladderTag = haikuAlias, haikuAlias
		// Ascending rungs from whichever workers came up (0.8B -> 2B -> 4B -> main), with
		// ascending load thresholds; the last rung is the catch-all. A subagent starts on
		// the smallest rung and climbs as its estimated load grows.
		type rung struct {
			url, think string
			tools      []string
		}
		var rungs []rung
		if haikuURL != "" {
			rungs = append(rungs, rung{haikuURL, "low", workerTools})
		}
		if midURL != "" {
			rungs = append(rungs, rung{midURL, "low", workerTools})
		}
		if sonnetURL != "" {
			rungs = append(rungs, rung{sonnetURL, "low", sonnetTools})
		}
		if mainEscalate {
			rungs = append(rungs, rung{mainURL, "", nil}) // HEAD model: keep all tools
		}
		thresholds := []int{2048, 6144, 16384, 49152}
		for i, r := range rungs {
			max := catchAll
			if i < len(rungs)-1 && i < len(thresholds) {
				max = thresholds[i]
			}
			ladder = append(ladder, router.Tier{Upstream: r.url, Think: r.think, MaxEstTokens: max, Tools: r.tools})
		}
	}

	// The model-aware router is mandatory in team mode -- it dispatches every request to
	// its backend (and applies adaptive reasoning where appropriate).
	rtr, err := router.StartTeam(cfg, routes, ladder, ladderTag, mainURL)
	if err != nil {
		ui.Err("team router failed: %v", err)
		return 1
	}
	defer rtr.Stop()
	baseURL := rtr.BaseURL()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; stopAll(); os.Exit(130) }()

	maxOut := engine.ResolveMaxOutput(cfg, headCtx)
	switch {
	case sub == "tiered":
		ui.Good("team ready  main=%s  sonnet=%s  haiku=%s", slots.Opus, slots.Sonnet, slots.Haiku)
		ui.Info("tiered: per-agent pins (research->haiku, collator/review->sonnet); generic/Workflow agents inherit main")
		if slots.Sonnet == mainAlias && slots.Haiku == mainAlias {
			ui.Warn("no workers running - every tier falls back to the main model")
		}
	case len(ladder) == 0:
		ui.Warn("team: no worker started - subagents fall back to the main model (see the worker logs)")
	case sub == "dynamic":
		top := haikuAlias
		if midURL != "" {
			top = cfg.Team.Mid
		}
		if sonnetURL != "" {
			top = slots.Sonnet
		}
		if mainEscalate {
			top = mainAlias
		}
		ui.Good("team ready  main=%s  subagents start on %s, escalate by load up to %s", mainAlias, haikuAlias, top)
		ui.Info("every subagent + Workflow fan-out starts small and escalates by request load; main orchestrates")
	default: // haiku / sonnet single-tier
		ui.Good("team ready  main=%s  all subagents -> %s", mainAlias, subagentModel)
	}
	if !agent.Available(app) {
		ui.Warn("%s not found on PATH - install it, then re-run.", app)
	}
	env := agent.Env(baseURL, slots, maxOut, headCtx, mainAlias, subagentModel) // pin main + force subagents onto the worker(s)
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
	// Extend prompt-prefix cache reuse (recovers the cache after small mid-prompt edits);
	// probed, so an older engine that lacks the flag just runs without it.
	args = append(args, engine.CacheReuseArgs(serverBin)...)
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

// midEnabled reports whether the optional dynamic-mode middle rung is configured (a model
// alias), as opposed to disabled via "off"/"none"/"false"/empty.
func midEnabled(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "none", "false":
		return false
	}
	return true
}

func gpuOrCPU(cfg *config.Config, hw platform.Hardware) string {
	if engine.GpuLayers(cfg, hw) > 0 {
		return "GPU"
	}
	return "CPU"
}

// wantTeam decides whether to run team mode. Explicit flags win (--noteam off, --team on);
// otherwise [team].mode governs: "off" never, "on" always, "auto" (default) engages when
// the main model is big enough AND there's RAM for the workers. Team's tier env is Claude
// Code-specific, so other apps stay single.
func wantTeam(app string, teamFlag, noteamFlag bool, cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware, model string) bool {
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
		return teamWorthwhile(cfg, cat, hw, model)
	}
}

// teamWorthwhile reports whether team should auto-engage for a model: the main model is
// ABOVE THE NANO TIER (the tier the CPU workers themselves come from, so offloading
// subagents to them is worthwhile) AND there's RAM for at least the SMALLEST worker. The
// worker set is then fit to RAM at launch (smallest-first); only a model that can't host
// even the smallest worker falls back to a single model.
func teamWorthwhile(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware, model string) bool {
	if !aboveNanoTier(cfg, cat, model) {
		return false
	}
	mb, path := mainModelSize(cfg, cat, model)
	return smallestWorkerRAMMB(cfg, cat) <= workerRAMBudgetMB(cfg, hw, path, mb)
}

// aboveNanoTier reports whether the main model is bigger than the nano tier: a catalogued
// model by its tier, an uncatalogued downloaded model by size (the largest nano model is
// only ~3 GB, so >=4 GB is comfortably above it).
func aboveNanoTier(cfg *config.Config, cat *catalog.Catalog, model string) bool {
	if m := cat.Find(model); m != nil {
		return !strings.EqualFold(m.Tier, "nano")
	}
	if p, _ := downloadedPath(cfg, cat, model); p != "" {
		return engine.FileMB(p) >= 4000
	}
	return false
}

// mainModelSize returns the main model's size in MB (its on-disk size if downloaded, else
// the catalogue estimate) and its path ("" if not downloaded).
func mainModelSize(cfg *config.Config, cat *catalog.Catalog, model string) (mb int, path string) {
	if p, _ := downloadedPath(cfg, cat, model); p != "" {
		if m := engine.FileMB(p); m > 0 {
			return m, p
		}
	}
	if m := cat.Find(model); m != nil {
		return sizeStrToMB(m.Size), ""
	}
	return 0, ""
}

// workerRAMBudgetMB is the system RAM available for CPU workers after the main model's own
// RAM use -- its full footprint on Apple unified memory or when MoE experts are offloaded;
// otherwise just runtime overhead -- and OS headroom. Huge (no limit) when RAM is unknown.
func workerRAMBudgetMB(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB int) int {
	if hw.RAMMB <= 0 {
		return 1 << 30
	}
	mainRAM := 1024 // runtime overhead when the model sits in discrete VRAM
	if hw.Unified || (modelPath != "" && engine.WillOffloadExperts(cfg, hw, modelPath)) {
		mainRAM = modelMB // shared unified pool, or MoE experts offloaded to RAM
	}
	const osHeadroomMB = 2048
	if b := hw.RAMMB - mainRAM - osHeadroomMB; b > 0 {
		return b
	}
	return 0
}

// workerRAMMB estimates the RAM one CPU worker needs: its weights (catalogue size) plus a
// KV-cache / runtime margin.
func workerRAMMB(cat *catalog.Catalog, alias string) int {
	size := 0
	if m := cat.Find(alias); m != nil {
		size = sizeStrToMB(m.Size)
	}
	if size == 0 {
		size = 1500 // unknown (e.g. a custom worker) -> rough estimate
	}
	return size + 512
}

// smallestWorkerRAMMB is the RAM the smallest configured worker needs -- the cheapest rung
// team could run, used to decide team-vs-single.
func smallestWorkerRAMMB(cfg *config.Config, cat *catalog.Catalog) int {
	min := 0
	for _, a := range []string{cfg.Team.Haiku, cfg.Team.Mid, cfg.Team.Sonnet} {
		a = strings.TrimSpace(a)
		if a == "" || strings.EqualFold(a, "off") || strings.EqualFold(a, "none") {
			continue
		}
		if r := workerRAMMB(cat, a); min == 0 || r < min {
			min = r
		}
	}
	if min == 0 {
		min = 1024
	}
	return min
}
