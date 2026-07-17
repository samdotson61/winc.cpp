package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
// as the main agent on the GPU, and small worker models run mapped onto Claude
// Code's sonnet (collator/review) and haiku (research fan-out + Explore) tiers.
// A model-aware router fans each tier's requests to the right backend. The head
// takes VRAM precedence absolutely: it loads first and takes everything it wants;
// workers then claim only the measured leftover VRAM (largest worker first) and
// otherwise run on the CPU, so they can never shrink the head's context.
func startTeam(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware, app, mainQuery string) int {
	mainPath, mainAlias := downloadedPath(cfg, cat, mainQuery)
	if mainPath == "" {
		reportMissingModel(mainAlias, mainQuery)
		return 1
	}
	rememberLastUsed(cfg, app, mainAlias)
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

	// Dynamic mode may escalate heavy subagents onto the main GPU model -- but only
	// when there's VRAM headroom to serve them concurrently. No --parallel flag is
	// passed: the engine's auto mode runs a UNIFIED KV pool (verified on the shipped
	// engine: n_parallel auto + kv_unified, every sequence may use the FULL window),
	// so the head keeps its whole context and an escalated subagent borrows from the
	// shared pool only while it actually runs. The old explicit --parallel 2 HALVED
	// the head's window permanently -- even with zero subagents active.
	mainEscalate := sub == "dynamic" && engine.MainEscalationOK(cfg, hw, mainPath)

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
	// Unified KV pool: the head's window is the WHOLE loaded window (escalated
	// subagents share the pool transiently; exhaustion surfaces as an ordinary
	// context error the router already rewrites into the compaction signal).
	headCtx := loadedCtx
	// Provision the agent-side notes: the REAL window (the agent's own UI cannot
	// show local windows below 100k), measured speeds, and small-window practices.
	if err := config.WriteAgentNotes(loadedCtx, headCtx, lastBench.gen, lastBench.pp); err != nil {
		ui.Warn("could not write agent notes: %v", err)
	}

	serverBin := engine.LlamaServerPath()
	workerPar, workerCtx, sonnetPar, sonnetCtx := workerGeometry(cfg.Team.Parallel, hw)
	if smallRAM(hw) {
		ui.Dim("team: small RAM (%d MB) - halving worker fan-out for double per-agent context (research %dx%d, collator %dx%d)",
			hw.RAMMB, workerPar, workerCtx/workerPar, sonnetPar, sonnetCtx/sonnetPar)
	}

	// Workers default to the CPU so they never shrink the head's VRAM -- but VRAM
	// the head left on the table is free speedup. The head is already resident (it
	// loaded first, with everything it wanted), so the leftover is MEASURED from the
	// cards, not estimated. Hand it out largest worker first: the 4B is the ladder's
	// information-agent catch-all, so it benefits most from GPU decode.
	gpuLeft := leftoverVRAMMB()
	// Sanity-check the leftover before spending it: a head the driver silently
	// placed in shared system memory leaves dedicated VRAM looking untouched, so
	// the "leftover" reading is really the head's own unclaimed seat -- seating
	// workers in it evicts the head for good (observed live: ~24 GB of phantom
	// leftover with a 19 GB head supposedly resident). If the arithmetic says the
	// head can't be in dedicated memory, no worker touches the GPU.
	if headMB := engine.FileMB(mainPath); gpuLeft > 0 && headMB > 0 && engine.ForcedFullGPU(cfg, hw, mainPath) {
		preHead := 0
		for _, g := range hw.GPUs {
			preHead += g.FreeMB
		}
		if preHead > 0 && gpuLeft > preHead-headMB/2 {
			ui.Warn("team: the head does not look resident in dedicated VRAM (%d MB free after loading a %d MB model into %d MB) - workers stay on CPU", gpuLeft, headMB, preHead)
			gpuLeft = 0
		}
	}
	if gpuLeft > 0 {
		ui.Dim("team: ~%d MB VRAM left over after the head - workers that fit will use it (largest first)", gpuLeft)
	}
	claimGPU := func(path string, ctx int) int {
		need := workerGPUNeedMB(path, ctx)
		if need <= gpuLeft {
			gpuLeft -= need
			return need
		}
		return 0
	}

	// Launch the worker(s) as records, so the head re-check below can demote GPU
	// claimants to the CPU and the tier mapping reflects the final reality.
	type workerLaunch struct {
		run                   bool
		path, alias, role, lp string
		par, ctx              int
		proc                  *server.Proc
		url                   string
		onGPU                 bool
	}
	ws := []*workerLaunch{
		{run: runSonnet, path: sonnetPath, alias: sonnetAlias, role: "sonnet", lp: filepath.Join(logDir, "worker-sonnet.log"), par: sonnetPar, ctx: sonnetCtx},
		{run: runMid, path: midPath, alias: midAlias, role: "mid", lp: filepath.Join(logDir, "worker-mid.log"), par: workerPar, ctx: workerCtx},
		{run: runHaiku, path: haikuPath, alias: haikuAlias, role: "haiku", lp: filepath.Join(logDir, "worker-haiku.log"), par: workerPar, ctx: workerCtx},
	}
	anyOnGPU := false
	for _, w := range ws {
		if !w.run || w.path == "" {
			continue
		}
		if p, url, _, gpu := startWorker(cfg, hw, serverBin, w.path, w.alias, mainAlias, w.par, w.ctx, w.role, w.lp, claimGPU(w.path, w.ctx)); p != nil {
			procs = append(procs, p)
			w.proc, w.url, w.onGPU = p, url, gpu
			anyOnGPU = anyOnGPU || gpu
		}
	}

	// The head was verified GPU-resident when it loaded -- but worker GPU claims
	// land AFTER that, and the driver satisfies a new allocation by evicting
	// whatever it must. If any worker took VRAM, re-measure the head's prompt
	// speed; a degraded head means the workers' speedup is costing far more than
	// it gives, so those workers move back to the CPU (they were started this
	// session, so stopping them is winc's to do).
	if anyOnGPU {
		if pp, _, measured, slow := benchServer(mainURL); slow || (measured && pp < ppHealthyFloor) {
			ui.Warn("team: head prompt processing degraded to ~%.0f tok/s after workers claimed VRAM - moving those workers to the CPU", pp)
			for _, w := range ws {
				if w.proc == nil || !w.onGPU {
					continue
				}
				w.proc.Stop()
				w.proc, w.url, w.onGPU = nil, "", false
				if p, url, _, _ := startWorker(cfg, hw, serverBin, w.path, w.alias, mainAlias, w.par, w.ctx, w.role, w.lp, 0); p != nil {
					procs = append(procs, p)
					w.proc, w.url = p, url
				}
			}
		}
	}

	slots := agent.Slots{Opus: mainAlias, Sonnet: mainAlias, Haiku: mainAlias}
	var sonnetURL, midURL, haikuURL string
	var workers []teamWorker
	for _, w := range ws {
		if w.proc == nil {
			continue
		}
		workers = append(workers, teamWorker{proc: w.proc, name: w.role, url: w.url, logPath: w.lp})
		switch w.role {
		case "sonnet":
			sonnetURL, slots.Sonnet = w.url, w.alias
		case "mid":
			midURL = w.url // ladder-only rung; not mapped to a Claude Code tier
		case "haiku":
			haikuURL, slots.Haiku = w.url, w.alias
		}
	}

	// Build the dispatch: explicit per-agent routes (tiered), or a subagent tag + escalation
	// ladder (dynamic/haiku/sonnet) that forces every subagent (Task + Workflow) onto the
	// worker(s). The HEAD (pinned to the main model) always reaches the main backend.
	disp := buildDispatch(cfg, sub, slots, sonnetURL, haikuURL, midURL, mainURL, sonnetAlias, haikuAlias, mainEscalate)

	// The model-aware router is mandatory in team mode -- it dispatches every request to
	// its backend (and applies adaptive reasoning where appropriate).
	rtr, err := router.StartTeam(cfg, disp.routes, disp.ladder, disp.ladderTag, mainURL, headCtx)
	if err != nil {
		ui.Err("team router failed: %v", err)
		return 1
	}
	defer rtr.Stop()
	baseURL := rtr.BaseURL()
	if rtr.LogPath() == "" {
		ui.Dim("note: could not create the router log - routing diagnostics are off this session")
	}

	// Watch each worker for the life of the session: one that exits or stops answering
	// /health gets its rung marked dead, so the ladder re-routes around it instead of
	// feeding it requests that fail (which Claude Code would misread as the model being
	// unavailable). Detection only -- winc never kills or restarts anything.
	var teardown atomic.Bool
	for _, w := range workers {
		go watchWorker(rtr, w, &teardown)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; teardown.Store(true); stopAll(); os.Exit(130) }()

	maxOut := engine.ResolveMaxOutput(cfg, headCtx)
	switch {
	case sub == "tiered":
		ui.Good("team ready  main=%s  sonnet=%s  haiku=%s", slots.Opus, slots.Sonnet, slots.Haiku)
		ui.Info("tiered: per-agent pins (research->haiku, collator/review->sonnet); generic/Workflow agents inherit main")
		if slots.Sonnet == mainAlias && slots.Haiku == mainAlias {
			ui.Warn("no workers running - every tier falls back to the main model")
		}
	case len(disp.ladder) == 0:
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
		ui.Good("team ready  main=%s  all subagents -> %s", mainAlias, disp.subagentModel)
	}
	if !agent.Available(app) {
		ui.Warn("%s not found on PATH - install it, then re-run.", app)
	}
	env := agent.Env(baseURL, slots, maxOut, headCtx, mainAlias, disp.subagentModel) // pin main + force subagents onto the worker(s)
	ui.Good("launching %s ... (Ctrl-C to stop)", app)
	if err := agent.Launch(app, env); err != nil {
		ui.Warn("agent exited: %v", err)
	}
	teardown.Store(true) // watchdogs stand down; worker deaths from here on are expected
	reportTeamStats(rtr)
	return 0
}

// dispatch is the request-routing plan startTeam hands the router: explicit per-model
// routes (tiered mode) or an escalation ladder behind a subagent tag (dynamic/forced).
type dispatch struct {
	routes        []router.Route
	ladder        []router.Tier
	ladderTag     string // the model tag subagent requests carry ("" = no ladder)
	subagentModel string // the model alias Claude Code's subagents are pinned to
}

// buildDispatch translates the subagent policy + whichever workers actually came up into
// the router's dispatch plan. Extracted from startTeam so the policy switch is testable
// without launching servers.
func buildDispatch(cfg *config.Config, sub string, slots agent.Slots, sonnetURL, haikuURL, midURL, mainURL, sonnetAlias, haikuAlias string, mainEscalate bool) dispatch {
	const catchAll = 1 << 30
	var d dispatch
	// Per-tier tool allowlists: tiny workers (0.8B/2B) stay research-only; the 4B also gets
	// Write (collation/review); the HEAD model keeps every tool (nil = no stripping).
	workerTools := cfg.Team.WorkerTools
	sonnetTools := cfg.Team.SonnetTools
	// Per-tier generation caps (loop guard): the tiny research workers can run away and burn
	// minutes of CPU generating until they hit the context wall; the router lowers an
	// over-large max_tokens to these. The HEAD model is never capped.
	workerCap := cfg.Team.WorkerMaxTokens
	sonnetCap := cfg.Team.SonnetMaxTokens
	switch sub {
	case "tiered": // per-agent pins; generic/Workflow agents inherit the main model
		if sonnetURL != "" {
			d.routes = append(d.routes, router.Route{Name: "sonnet", Model: slots.Sonnet, Upstream: sonnetURL, Think: "", Tools: sonnetTools, MaxTokens: sonnetCap})
		}
		if haikuURL != "" {
			d.routes = append(d.routes, router.Route{Name: "haiku", Model: slots.Haiku, Upstream: haikuURL, Think: "low", Tools: workerTools, MaxTokens: workerCap})
		}
	case "sonnet": // force all subagents to the 4B
		d.subagentModel, d.ladderTag = sonnetAlias, sonnetAlias
		if sonnetURL != "" {
			d.ladder = []router.Tier{{Name: "sonnet", Upstream: sonnetURL, Think: "", MaxEstTokens: catchAll, Tools: sonnetTools, MaxTokens: sonnetCap}}
		}
	case "haiku": // force all subagents to the 0.8B
		d.subagentModel, d.ladderTag = haikuAlias, haikuAlias
		if haikuURL != "" {
			d.ladder = []router.Tier{{Name: "haiku", Upstream: haikuURL, Think: "low", MaxEstTokens: catchAll, Tools: workerTools, MaxTokens: workerCap}}
		}
	default: // dynamic: tag every subagent with the haiku alias, escalate by request load
		d.subagentModel, d.ladderTag = haikuAlias, haikuAlias
		// Ascending rungs from whichever workers came up (0.8B -> 2B -> 4B -> main), with
		// ascending load thresholds; the last rung is the catch-all. A subagent starts on
		// the smallest rung and climbs as its estimated load grows.
		type rung struct {
			name, url, think string
			tools            []string
			cap              int
			head             bool
		}
		var rungs []rung
		if haikuURL != "" {
			rungs = append(rungs, rung{"haiku", haikuURL, "low", workerTools, workerCap, false})
		}
		if midURL != "" {
			rungs = append(rungs, rung{"mid", midURL, "low", workerTools, workerCap, false})
		}
		if sonnetURL != "" {
			rungs = append(rungs, rung{"sonnet", sonnetURL, "low", sonnetTools, sonnetCap, false})
		}
		if mainEscalate {
			// HEAD model: keep all tools, no cap. Marked Head so information-only
			// subagents (read/search/fetch) top out at the largest worker instead of
			// opening a second full session on the big GPU model.
			rungs = append(rungs, rung{"escalated", mainURL, "", nil, 0, true})
		}
		thresholds := []int{2048, 6144, 16384, 49152}
		for i, r := range rungs {
			max := catchAll
			if i < len(rungs)-1 && i < len(thresholds) {
				max = thresholds[i]
			}
			d.ladder = append(d.ladder, router.Tier{Name: r.name, Upstream: r.url, Think: r.think, MaxEstTokens: max, Tools: r.tools, MaxTokens: r.cap, Head: r.head})
		}
	}
	return d
}

// teamWorker is one launched CPU worker, as the watchdog sees it.
type teamWorker struct {
	proc    *server.Proc
	name    string // rung name: "haiku" | "mid" | "sonnet"
	url     string
	logPath string
}

// watchWorker monitors one CPU worker for the life of the session: a worker that exits
// or stops answering /health gets its rung marked dead in the router, so the ladder
// re-routes around it. Detection only -- winc never kills or restarts anything. Events
// go to the router log, never the shared terminal (the agent's TUI owns it); the
// end-of-session summary surfaces any deaths. Stands down once teardown begins.
func watchWorker(rtr *router.Router, w teamWorker, teardown *atomic.Bool) {
	const (
		interval    = 15 * time.Second
		maxFailures = 3 // consecutive /health failures before the rung is declared dead
	)
	failures := 0
	for {
		time.Sleep(interval)
		if teardown.Load() {
			return // session is shutting down; worker deaths from here on are expected
		}
		if w.proc.Dead() {
			rtr.MarkDead(w.name, w.url)
			return
		}
		if server.HealthOK(w.url) {
			failures = 0
			continue
		}
		failures++
		if failures >= maxFailures {
			rtr.MarkDead(w.name, w.url)
			return
		}
	}
}

// reportTeamStats prints the router's end-of-session summary -- only after the agent
// has exited, so nothing ever prints into its TUI mid-session.
func reportTeamStats(rtr *router.Router) {
	st := rtr.Stats()
	if len(st.Requests) == 0 && st.Overflows == 0 && len(st.Dead) == 0 {
		return
	}
	ui.Info("router session: %s", st)
	if len(st.Dead) > 0 {
		ui.Warn("team: worker(s) died mid-session: %s - their rungs fell back (see the worker logs)", strings.Join(st.Dead, ", "))
	}
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

// leftoverVRAMMB re-probes the GPUs once the head model is resident and returns the
// VRAM still free across all cards, minus a per-GPU safety margin (the head's CUDA
// workspaces grow a little under load, and we must NEVER make the head compete).
// 0 when the cards can't be probed (non-NVIDIA paths) -- workers then stay on CPU.
func leftoverVRAMMB() int {
	total := 0
	for _, g := range platform.ProbeGPUFree() { // memory snapshot only -- the GPU list hasn't changed since launch
		if free := g.FreeMB - 768; free > 0 {
			total += free
		}
	}
	return total
}

// workerGPUNeedMB estimates the VRAM a worker would occupy: weights + KV cache for
// its context window (q8, ~64 tokens/MB) + a compute buffer. Unknown sizes never
// claim GPU.
func workerGPUNeedMB(modelPath string, ctx int) int {
	mb := engine.FileMB(modelPath)
	if mb <= 0 {
		return 1 << 30
	}
	return mb + ctx/64 + 512
}

// startWorker launches one worker (dense small model) and returns its proc, URL,
// alias, and whether it landed on the GPU -- or (nil,"","",false) when it has no
// model, no engine, an alias colliding with the main model, or it fails to come
// up. parallel slots serve concurrent subagents. gpuMB > 0 grants the worker that
// much of the VRAM the head left over: all its layers are forced onto the GPU,
// with a CPU relaunch as the fallback if the load fails. gpuMB == 0 is the
// classic CPU worker.
func startWorker(cfg *config.Config, hw platform.Hardware, serverBin, modelPath, alias, mainAlias string, parallel, ctx int, role, logPath string, gpuMB int) (*server.Proc, string, string, bool) {
	if modelPath == "" || serverBin == "" {
		return nil, "", "", false
	}
	if strings.EqualFold(alias, mainAlias) {
		ui.Dim("team: %s worker is the same model as main - skipping (no separate tier needed)", role)
		return nil, "", "", false
	}

	// One launch attempt. Worker config: GPU only within the granted leftover slice
	// (else -ngl 0 so it never touches the head's VRAM), and drop the main model's
	// draft/MTP/extra flags (they don't apply to the worker). Run the worker server
	// in adaptive reasoning so the router governs thinking per request (a low budget
	// for the research tier -- small models need a little thinking for tools).
	try := func(gpu bool) (*server.Proc, string) {
		pnum := freePort()
		if pnum == 0 {
			ui.Warn("team: no free port for the %s worker - that tier falls back to main", role)
			return nil, ""
		}
		wc := *cfg
		wc.Performance.GpuLayers = "0"
		if gpu {
			wc.Performance.GpuLayers = "99"
		}
		// Small models are the MOST sensitive to KV quantization, and worker windows
		// are tiny anyway -- pin q8_0 so a session-level downshift (the head's KV
		// upgrade probe mutates cfg) never degrades the workers.
		wc.Performance.CacheType = "q8_0"
		wc.Performance.DraftModel = ""
		wc.Performance.Mtp = "off"
		wc.Performance.ExtraServerArgs = nil
		wc.Reasoning.Mode = "adaptive"
		wc.General.Port = pnum

		args := engine.ServerArgs(&wc, hw, modelPath, pnum, "", ctx) // ServerArgs adds family-correct sampling
		args = append(args, "--parallel", strconv.Itoa(parallel))
		// Unified memory + CPU worker (-ngl 0): pin the worker to the efficiency
		// cores. CPU and GPU share one memory bus on Apple Silicon, and GPU decode
		// is bandwidth-bound, so a worker decoding on the P cores steals bandwidth
		// from the main GPU model. MEASURED (M4 Pro, v1.27.0): concurrent CPU
		// workers cost the main model 16% decode; pinning them to the E cores
		// recovers about half. No-op for GPU workers and non-unified machines.
		if !gpu && hw.Unified {
			if lo, hi, ok := platform.EfficiencyCoreRange(); ok {
				args = append(args, "--cpu-range", fmt.Sprintf("%d-%d", lo, hi), "--threads", strconv.Itoa(hi-lo+1))
			}
		}
		proc, err := server.Start(serverBin, args, logPath)
		if err != nil {
			ui.Warn("team: %s worker failed to launch: %v", role, err)
			return nil, ""
		}
		where := "CPU"
		if gpu {
			where = fmt.Sprintf("GPU (~%d MB of leftover VRAM)", gpuMB)
		}
		url := fmt.Sprintf("http://%s:%d", cfg.General.Host, pnum)
		ui.Info("team: %s %s on %s (port %d, %d slots)", role, alias, where, pnum, parallel)
		if !server.WaitReady(url, "/health", 180*time.Second, proc.Dead) {
			proc.Stop()
			return nil, ""
		}
		return proc, url
	}

	if gpuMB > 0 {
		if proc, url := try(true); proc != nil {
			return proc, url, alias, true
		}
		ui.Dim("team: %s worker didn't come up on the GPU - retrying on the CPU", role)
	}
	if proc, url := try(false); proc != nil {
		return proc, url, alias, false
	}
	ui.Warn("team: %s worker didn't become ready - tier falls back to main; see %s", role, logPath)
	return nil, "", "", false
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

// Auto team needs hardware that can absorb 1-3 extra model servers without
// dragging the whole box: above the 16 GB discrete class (a 16 GB card reports
// ~16.3 GB; the next class up is 20-24 GB) and above the 24 GB unified class
// (a 24 GB Mac budgets ~17 GB for the GPU working set, and CPU workers then
// fight the head for the same pool). Below these, the head model alone is the
// right load. Explicit --team / [team] mode="on" always wins.
const (
	teamAutoMinVRAMMB    = 17408 // strictly above the 16 GB discrete class
	teamAutoMinUnifiedMB = 25600 // strictly above the 24 GB unified class
)

// teamAutoHardwareOK reports whether this machine is in the class where team
// mode auto-engages. CPU-only boxes (no VRAM) never auto-team: head + workers
// all compete for the same cores and RAM bandwidth.
func teamAutoHardwareOK(hw platform.Hardware) bool {
	if hw.Unified {
		return hw.RAMMB > teamAutoMinUnifiedMB
	}
	return hw.VRAMMB > teamAutoMinVRAMMB
}

// wantTeam decides whether to run team mode. Explicit flags win (--noteam off, --team on);
// otherwise [team].mode governs: "off" never, "on" always, "auto" (default) engages when
// the hardware is above the 16 GB discrete / 24 GB unified class AND the main model is
// big enough AND there's RAM for the workers. Team's tier env is Claude Code-specific,
// so other apps stay single.
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
		return teamAutoHardwareOK(hw) && teamWorthwhile(cfg, cat, hw, model)
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

// smallRAMMB is the threshold below which a system counts as "small RAM" for worker
// geometry: 16GB plus a margin (16GB boxes report a little under or exactly 16384
// depending on firmware carve-outs; the next common size, 24GB, is far above it).
const smallRAMMB = 17408

// smallRAM reports whether this is a small-RAM (<=16GB) system. Unknown RAM (0) is
// not small -- consistent with workerRAMBudgetMB treating unknown as unlimited.
func smallRAM(hw platform.Hardware) bool {
	return hw.RAMMB > 0 && hw.RAMMB <= smallRAMMB
}

// workerGeometry sizes each worker's --parallel slot count and total context window
// (llama-server splits the window evenly across slots). cfgPar is [team].parallel
// (<=0 -> default 4): the research workers (haiku/mid) get cfgPar slots x 8192
// tokens each; the sonnet collator gets 2 x 16384. On small-RAM systems (<=16GB)
// every worker keeps its total window but halves its slot count, doubling per-slot
// context: small boxes pair weak CPUs with the same 8192-token slots, so research
// agents overflow, escalate to rungs that CPU serves slowly, and contend for cores.
// Fewer, deeper slots let each agent actually finish. RAM use is unchanged -- the
// KV cache is sized by the total window, not the slot count.
func workerGeometry(cfgPar int, hw platform.Hardware) (workerPar, workerCtx, sonnetPar, sonnetCtx int) {
	if cfgPar <= 0 {
		cfgPar = 4
	}
	workerPar, workerCtx = cfgPar, cfgPar*8192
	sonnetPar, sonnetCtx = 2, 32768
	if smallRAM(hw) {
		workerPar = (workerPar + 1) / 2
		sonnetPar = 1
	}
	return
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
