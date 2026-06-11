package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/server"
	"winc/internal/ui"
)

// waitVRAMFree gives the driver a moment to finish releasing a just-stopped
// server's VRAM before the next big allocation: process exit is NOT memory
// release, and a relaunch that jumps straight to a large context lands on a
// half-drained card and OOMs (observed live: the same config failed immediately
// after a stop and loaded fine seconds later). Polls until enough is free for
// the model + runtime or the timeout passes; non-NVIDIA probes (no per-GPU
// data) return immediately, as does a machine that simply doesn't have the
// VRAM (partial offload is a legitimate state, not a wait condition).
func waitVRAMFree(neededMB int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	lastFree := -1
	for time.Now().Before(deadline) {
		gpus := platform.ProbeGPUFree() // memory snapshot only -- not a full re-detection per poll
		if len(gpus) == 0 {
			return
		}
		free, total := 0, 0
		for _, g := range gpus {
			free += g.FreeMB
			total += g.TotalMB
		}
		need := neededMB
		if need > total-512 {
			need = total - 512 // bigger than the cards -> partial offload; don't wait for the impossible
		}
		// Clearly clean cards: go immediately. Otherwise wait for the drain to
		// STABILIZE, not just cross the total -- the forced multi-GPU split follows
		// the free-VRAM ratio at launch, and a mid-drain snapshot skews it so one
		// card gets overpacked even when the total looks sufficient.
		if free >= need+4096 {
			return
		}
		if free >= need && lastFree >= 0 && free-lastFree < 256 {
			return
		}
		lastFree = free
		time.Sleep(2 * time.Second)
	}
}

// tryContextOnce launches llama-server at exactly one context size. Returns nil
// when it doesn't come up -- or when it comes up but the placement gate finds it
// isn't actually GPU-resident (the caller steps down exactly as for a failed
// load). lastResort marks a load that must never be rejected outright (the
// ladder's floor rung, or a reload of a rung that already passed): gate failures
// there warn loudly and accept, so the gate can never block a launch. gated says
// the USER's sizing was auto -- it must be computed against the original config
// by the top of the launch, because the ladder/probe/memo paths derive configs
// with the context or cache type PINNED for the attempt (those pins are still
// winc-chosen sizing, not user settings, and skipping the gate for them is what
// let a sysmem-paged 98304 probe rung get recorded as measured-good).
func tryContextOnce(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string, ctx int, noMTP, lastResort, gated bool) *server.Proc {
	waitVRAMFree(engine.FileMB(modelPath)+2048, 20*time.Second)
	// The gate covers exactly the loads where winc pins -ngl 99 because the model
	// SHOULD fit: those are the ones the driver can silently satisfy from shared
	// system memory when the pin turns out to be over budget. Explicit user
	// settings run as written.
	gate := gated && engine.ForcedFullGPU(cfg, hw, modelPath)
	preFree := 0
	if gate {
		for _, g := range platform.ProbeGPUFree() {
			preFree += g.FreeMB
		}
	}
	args := engine.ServerArgs(cfg, hw, modelPath, port, "", ctx)
	if !noMTP {
		args = append(args, engine.MTPArgs(cfg, hw, modelPath, serverBin)...) // MTP variant -> --spec-type draft-mtp (if supported)
	}
	args = append(args, engine.CacheReuseArgs(serverBin)...) // extend prompt-cache reuse (probed)
	lastBench = benchResult{}
	proc, err := server.Start(serverBin, args, logPath)
	if err != nil {
		return nil
	}
	if !server.WaitReady(serverURL, "/health", 240*time.Second, proc.Dead) {
		proc.Stop() // didn't fit / failed
		return nil
	}
	if gate {
		ok := verifyOnGPU(serverURL, modelPath, ctx, preFree, lastResort)
		lastBench.tried = true
		if !ok {
			proc.Stop()
			return nil
		}
	}
	return proc
}

// rungWorthTrying consults the engine's fit calculator (seconds, metadata only)
// before paying a weight upload for a rung. Rungs the calculator says need CPU
// spill are skipped outright. For MTP models -- whose draft context the
// calculator cannot see (~2 GB at large windows: its own cache plus compute
// buffers) -- the full-fit verdict must hold `margin` rungs higher: 2 for cold
// standalone ladder loads, 1 for probe rungs (which load right after a smaller
// server stopped, a measurably friendlier VRAM state). These margins reproduce
// every measured outcome on real hardware. The floor rung, partial-fit models,
// unified memory, and a missing tool always return true: the attempt itself
// remains the ground truth.
func rungWorthTrying(cfg *config.Config, hw platform.Hardware, modelPath string, ctx, margin int, isFloor bool) bool {
	if isFloor || hw.Unified || len(hw.GPUs) == 0 {
		return true
	}
	mb := engine.FileMB(modelPath)
	if mb <= 0 || engine.WillOffloadExperts(cfg, hw, modelPath) {
		return true
	}
	// The oracle's verdict is "can this stay FULLY on GPU" -- only meaningful for
	// loads winc will pin there. A partial fit (tiny card, model near the card's
	// size) is SUPPOSED to spill layers; vetoing its rungs for not being fully
	// resident drove 4 GB cards down to unusable windows. The attempt itself is
	// the ground truth, and a failed small-model load costs seconds, not minutes.
	if !engine.ForcedFullGPU(cfg, hw, modelPath) {
		return true
	}
	ct := engine.EffectiveCacheType(cfg, hw, modelPath, mb, false)
	probeCtx := ctx
	if engine.MTPActive(cfg, modelPath) {
		for i := 0; i < margin; i++ {
			probeCtx = engine.NextLadderRung(probeCtx)
		}
	}
	full, ok := engine.FitVerdictFull(cfg, modelPath, probeCtx, ct)
	if !ok {
		return true
	}
	if !full {
		ui.Dim("skipping the %d-token window (the engine fit says it can't stay fully on GPU)", ctx)
	}
	return full
}

// tryContextLadder launches llama-server at the most liberal context that fits,
// silently stepping down if a size fails to load. Returns (proc, ctx) or (nil, 0).
func tryContextLadder(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string, noMTP, gated bool) (*server.Proc, int) {
	target := engine.ResolveContext(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
	rungs := engine.ContextLadder(target)
	for i, ctx := range rungs {
		if !rungWorthTrying(cfg, hw, modelPath, ctx, 2, i == len(rungs)-1) {
			continue
		}
		if proc := tryContextOnce(cfg, hw, modelPath, serverBin, port, serverURL, logPath, ctx, noMTP, i == len(rungs)-1, gated); proc != nil {
			return proc, ctx
		}
	}
	return nil, 0
}

// startLlamaFitting ensures a *working* engine backend and launches it. If the
// installed backend won't run here (e.g. a CUDA build whose PTX the driver is too
// old for), it silently falls back to the next backend (cuda -> vulkan -> cpu),
// re-downloading as needed, then launches at the largest context that fits.
// Returns (proc, loadedCtx) or (nil, 0).
func startLlamaFitting(cfg *config.Config, hw platform.Hardware, modelPath string, port int, serverURL, logPath string) (*server.Proc, int) {
	exclude := map[string]bool{}
	unknownCleared := false
	for {
		bin, backend, err := engine.AcquireLlamaExcluding(hw, exclude)
		if err != nil {
			ui.Err("could not get a working llama.cpp backend: %v", err)
			return nil, 0
		}
		ui.Info("loading model + waiting for server (%s)...", backendLabel(backend))
		wasAuto := autoSized(cfg)
		fp := launchFingerprint(cfg, hw) // against the ORIGINAL config, before launch mutations
		// Fast path: a previous session already measured this model's best window and
		// KV cache under these exact sizing inputs -- load straight there (one load
		// instead of re-walking the ladder).
		if wasAuto {
			if mctx, mct, _, mpl := loadLaunchMemo(modelPath, fp); mctx > 0 {
				lc := *cfg
				lc.Performance.CacheType = mct
				// Replay the remembered PLACEMENT, not just the window: a spill result
				// must use engine placement (forcing residency would gate-reject it),
				// and a no-MTP result only fits because the draft allowance is gone.
				switch mpl {
				case plSpill:
					lc.Performance.GpuLayers = engine.GpuLayersEngine
				case plNoMTP:
					lc.Performance.Mtp = "off"
				}
				if proc := tryContextOnce(&lc, hw, modelPath, bin, port, serverURL, logPath, mctx, false, false, true); proc != nil {
					cfg.Performance.CacheType = mct
					if mpl == plNoMTP {
						cfg.Performance.Mtp = "off"
					}
					// The placement gate already measured this server (a remembered rung
					// gets re-verified every launch -- free VRAM drifts day to day); the
					// report and the memo reuse its numbers.
					pp, tps := launchBench(serverURL)
					reportDecode(pp, tps, mctx)
					saveLaunchMemo(modelPath, mctx, mct, tps, fp, mpl)
					return proc, mctx
				}
				// Some boundary windows only load via the probe's staircase (the forced
				// split follows the free-VRAM ratio, which a just-stopped smaller server
				// leaves in a friendlier shape -- measured). Re-enter the ladder ONE rung
				// below the remembered size and let the probe climb back, skipping the
				// jumbo rungs the original measurement already failed.
				ui.Dim("remembered launch (%d tokens, %s KV) didn't load standalone - re-measuring from one rung down...", mctx, mct)
				lc2 := *cfg
				lc2.Performance.Context = strconv.Itoa(ladderBelow(mctx))
				if proc, ctx := tryContextLadder(&lc2, hw, modelPath, bin, port, serverURL, logPath, false, true); proc != nil {
					lc2.Performance.Context = cfg.Performance.Context // restore auto for the probe's sizing math
					proc, ctx = upgradeLadderQ4(&lc2, hw, modelPath, bin, port, serverURL, logPath, proc, ctx, true)
					placement := plGPU
					if proc != nil {
						cfg.Performance.CacheType = lc2.Performance.CacheType
						proc, ctx, placement = ensureBottomTarget(cfg, hw, modelPath, bin, port, serverURL, logPath, proc, ctx)
					}
					if proc != nil {
						pp, tps := launchBench(serverURL)
						reportDecode(pp, tps, ctx)
						ct := engine.EffectiveCacheType(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
						saveLaunchMemo(modelPath, ctx, ct, tps, fp, placement)
					}
					return proc, ctx
				}
				// Even the remembered range failed (hardware changed?) -> full flow below.
			}
		}
		if proc, ctx := tryContextLadder(cfg, hw, modelPath, bin, port, serverURL, logPath, false, wasAuto); proc != nil {
			proc, ctx = upgradeLadderQ4(cfg, hw, modelPath, bin, port, serverURL, logPath, proc, ctx, wasAuto)
			placement := plGPU
			if proc != nil && wasAuto {
				proc, ctx, placement = ensureBottomTarget(cfg, hw, modelPath, bin, port, serverURL, logPath, proc, ctx)
			}
			if proc != nil {
				pp, tps := launchBench(serverURL)
				reportDecode(pp, tps, ctx)
				if wasAuto {
					ct := engine.EffectiveCacheType(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
					saveLaunchMemo(modelPath, ctx, ct, tps, fp, placement)
				}
			}
			return proc, ctx
		}
		// A buggy MTP/draft path can stop the server from starting (or make it crash on
		// use). If MTP was actually active (baked-in heads or an external Gemma head),
		// retry this same backend once without it before blaming the backend -- the
		// model still runs, just without the speedup.
		if len(engine.MTPArgs(cfg, hw, modelPath, bin)) > 0 {
			ui.Warn("server didn't start with MTP - retrying without speculative decoding...")
			if proc, ctx := tryContextLadder(cfg, hw, modelPath, bin, port, serverURL, logPath, true, wasAuto); proc != nil {
				return proc, ctx
			}
		}
		// The backend didn't start at any context size -> it's incompatible here.
		if backend == "" {
			if unknownCleared {
				ui.Err("engine failed to start; see %s", logPath)
				return nil, 0
			}
			unknownCleared = true
			ui.Warn("installed engine didn't run here - switching to a compatible backend...")
			engine.ClearBinEngine()
			continue
		}
		exclude[backend] = true
		ui.Warn("%s backend isn't compatible here - trying another...", backend)
		engine.ClearBinEngine()
	}
}

// Launch placements, recorded in the memo so replays load the same way:
// plGPU = fully resident (forced -ngl 99, gate-verified); plNoMTP = fully
// resident with the MTP draft dropped to afford the window; plSpill = engine
// device placement, layers spilled to RAM for the window.
const (
	plGPU   = "gpu"
	plNoMTP = "nomtp"
	plSpill = "spill"
)

// ensureBottomTarget upgrades a launch that settled below the universal bottom
// target (~64k usable + the agent's fixed overhead, BottomCtxTokens total): the
// fully-resident ladder maxed out under it, so trade for the window in cost
// order. Stage 1, MTP models only: drop the speculative draft -- its context +
// buffers cost 1-2 GB at big windows and the loss is ~25-35% decode, and the
// result stays FULLY resident (gate-verified). Stage 2: the engine's device
// placement -- layers spill to RAM, measured 2-4x decode, but every agent gets
// a workable window; the decode report states the price. The resident server
// is restored when neither attempt loads (RAM-tight boxes). Only for
// winc-sized GPU launches; CPU-only and unified memory size their own way,
// and explicit settings never reach here.
func ensureBottomTarget(cfg *config.Config, hw platform.Hardware, modelPath, bin string, port int, serverURL, logPath string, proc *server.Proc, ctx int) (*server.Proc, int, string) {
	if proc == nil || ctx >= engine.BottomCtxTokens || hw.Unified || hw.VRAMMB <= 0 || len(hw.GPUs) == 0 {
		return proc, ctx, plGPU
	}
	stopped := false
	if len(engine.MTPArgs(cfg, hw, modelPath, bin)) > 0 {
		ui.Info("settled at %d - below the %d-token bottom target; trying the bottom without the MTP draft (frees its context + buffers, costs the speculative speedup)...", ctx, engine.BottomCtxTokens)
		proc.Stop()
		stopped = true
		nm := *cfg
		nm.Performance.Mtp = "off" // also zeroes the sizing allowance + the fit-oracle's draft margin
		if p2 := tryContextOnce(&nm, hw, modelPath, bin, port, serverURL, logPath, engine.BottomCtxTokens, false, false, true); p2 != nil {
			ui.Good("bottom target reached fully on GPU by dropping the MTP draft: %d tokens", engine.BottomCtxTokens)
			cfg.Performance.Mtp = "off"
			return p2, engine.BottomCtxTokens, plNoMTP
		}
	}
	if !stopped {
		ui.Info("settled at %d fully on GPU - below the %d-token bottom target; retrying with engine placement (layers may spill to RAM: slower, roomier)...", ctx, engine.BottomCtxTokens)
		proc.Stop()
	} else {
		ui.Info("retrying the bottom target with engine placement (layers may spill to RAM: slower, roomier)...")
	}
	sp := *cfg
	sp.Performance.GpuLayers = engine.GpuLayersEngine
	if p2 := tryContextOnce(&sp, hw, modelPath, bin, port, serverURL, logPath, engine.BottomCtxTokens, false, false, false); p2 != nil {
		ui.Good("engine placement reached the bottom target: %d tokens", engine.BottomCtxTokens)
		return p2, engine.BottomCtxTokens, plSpill
	}
	ui.Warn("the bottom-target attempts didn't load - keeping the %d-token fully-resident window", ctx)
	if p2 := tryContextOnce(cfg, hw, modelPath, bin, port, serverURL, logPath, ctx, false, true, true); p2 != nil {
		return p2, ctx, plGPU
	}
	ui.Err("engine did not come back after the bottom-target attempt; see %s", logPath)
	return nil, 0, plGPU
}

// ladderBelow is the standard rung below ctx (the ladder's second entry when ctx
// heads it), bottoming out at ctx itself.
func ladderBelow(ctx int) int {
	if l := engine.ContextLadder(ctx); len(l) > 1 {
		return l[1]
	}
	return ctx
}

// autoSized reports whether the context and KV cache are both winc-chosen. The
// launch memo only applies to winc-sized launches; explicit settings run as written.
func autoSized(cfg *config.Config) bool {
	ctxAuto := false
	switch strings.ToLower(strings.TrimSpace(cfg.Performance.Context)) {
	case "", "auto", "optimal":
		ctxAuto = true
	}
	ct := strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType))
	return ctxAuto && (ct == "" || ct == "auto")
}

// starvedKV is the KV cache the upgrade probe downshifts to: keys stay q8_0
// (4-bit keys measure ~+10% perplexity -- past the usefulness line for coding),
// values drop to q4_0 (near-lossless) -- ~1.3x the window at minimal quality cost.
const starvedKV = "q8_0/q4_0"

// benchResult is the placement gate's measurement for the accepted server,
// reused by the final speed report so a launch is never benched twice.
type benchResult struct {
	pp, gen  float64
	measured bool // the numbers are real (vs a failed measurement)
	tried    bool // the gate measured this server; the report must not re-bench
}

var lastBench benchResult

// ppHealthyFloor is the batched prompt-processing speed (tok/s) below which a
// forced-full-GPU load is treated as not actually GPU-resident. Fully resident
// models measure many hundreds on any hardware this policy covers; weights the
// driver paged to shared system memory measured 50-125 -- and decelerating --
// on real 16+12 GB hardware. 150 sits well clear of both.
const ppHealthyFloor = 150

// benchSickSecs: a bench request that drags past this before failing is itself
// the verdict -- ~2.5k prompt tokens + 48 generated takes single-digit seconds
// on any healthy GPU-resident server.
const benchSickSecs = 45

// benchServer measures real prompt-processing and decode speed with one fixed
// completion against the freshly-loaded (and warmed-up) server. The prompt is
// ~2.5k tokens so prompt processing runs in the batched regime (tiny prompts
// are overhead-bound and read 5-10x low). measured=false when speeds couldn't
// be determined (fast transport failure, or a response without timings) --
// callers never gate on a broken measurement. slow=true when the request itself
// dragged past benchSickSecs before failing.
func benchServer(serverURL string) (pp, gen float64, measured, slow bool) {
	words := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel india juliet ", 200)
	body, _ := json.Marshal(map[string]any{
		"messages":    []map[string]string{{"role": "user", "content": "Repeat the word alpha once. Ignore the rest: " + words}},
		"max_tokens":  48,
		"temperature": 0,
	})
	start := time.Now()
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Post(serverURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, 0, false, time.Since(start) > benchSickSecs*time.Second
	}
	defer resp.Body.Close()
	var out struct {
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Timings.PromptPerSecond <= 0 {
		return 0, 0, false, time.Since(start) > benchSickSecs*time.Second
	}
	return out.Timings.PromptPerSecond, out.Timings.PredictedPerSecond, true, false
}

// verifyOnGPU is the placement gate for forced-full-GPU loads. /health says
// nothing about WHERE the model landed: when a pinned -ngl 99 load exceeds free
// dedicated memory, the Windows driver can satisfy the allocations from SHARED
// system memory instead of failing (observed live: a 19 GB model "loaded" with
// both cards still ~93% free, ~20 GB committed in system RAM, health green, a
// tiny bench answering normally -- and the first real prompt crawling at 50-125
// tok/s, decelerating as the KV filled). Two measured checks catch it:
//   - residency: free dedicated VRAM must drop by at least HALF the model size
//     across the load. A resident model consumes at least its own weights, so
//     the loose bound cannot misfire on a healthy load.
//   - throughput: batched prompt processing must clear ppHealthyFloor.
//
// Rejecting returns false; the caller then steps the ladder down exactly as if
// the load had failed. lastResort loads always accept, warning loudly instead.
func verifyOnGPU(serverURL, modelPath string, ctx, preFreeMB int, lastResort bool) bool {
	sick := ""
	if mb := engine.FileMB(modelPath); residencyBroken(preFreeMB, postFreeMB(), mb) {
		sick = fmt.Sprintf("dedicated VRAM use rose by less than half the model's %d MB", mb)
	} else {
		pp, gen, measured, slow := benchServer(serverURL)
		lastBench = benchResult{pp: pp, gen: gen, measured: measured}
		if slow || (measured && pp < ppHealthyFloor) {
			sick = fmt.Sprintf("prompt processing measured ~%.0f tok/s (GPU-resident is %d+)", pp, ppHealthyFloor)
		} else if measured {
			ui.Dim("placement check: prompt ~%.0f tok/s at %d tokens - GPU-resident", pp, ctx)
		}
	}
	if sick == "" {
		return true
	}
	if lastResort {
		ui.Warn("the %d-token window loaded but is NOT GPU-resident: %s", ctx, sick)
		ui.Warn("  nothing smaller left to try - continuing; expect slow prompts (free some VRAM and relaunch to fix)")
		return true
	}
	ui.Warn("the %d-token window loaded but is not GPU-resident (%s) - stepping down", ctx, sick)
	return false
}

// postFreeMB is the combined free dedicated VRAM right now (0 = no probe data).
func postFreeMB() int {
	free := 0
	for _, g := range platform.ProbeGPUFree() {
		free += g.FreeMB
	}
	return free
}

// residencyBroken reports whether the free-VRAM drop across a load is too small
// for the model to actually be resident in dedicated memory. Missing probe data
// (either side) or an unknown model size never rejects -- only positive
// evidence does.
func residencyBroken(preFreeMB, postFreeMB, modelMB int) bool {
	if preFreeMB <= 0 || postFreeMB <= 0 || modelMB <= 0 {
		return false
	}
	return preFreeMB-postFreeMB < modelMB/2
}

// launchBench returns the launch's measured speeds: the placement gate's numbers
// when it ran (gated rungs are benched as part of acceptance -- never bench
// twice), one LIGHT decode-only measurement otherwise (explicit settings,
// unified memory, expert-offload, CPU: configs the gate doesn't cover). The
// gate's ~2.5k-token prompt is what makes its PP verdict meaningful -- but on a
// CPU-class box that prompt alone is a minute of launch time, and with no gate
// there's nothing to verify, only a decode speed to report.
func launchBench(serverURL string) (pp, gen float64) {
	if lastBench.tried {
		return lastBench.pp, lastBench.gen
	}
	return 0, benchDecodeSmall(serverURL)
}

// benchDecodeSmall measures decode speed with a tiny fixed completion (~25
// prompt tokens + 48 generated). Returns 0 when the request fails -- this is
// measurement for the launch report, never a gate.
func benchDecodeSmall(serverURL string) float64 {
	body := `{"messages":[{"role":"user","content":"Count from 1 to 30 as plain digits separated by spaces."}],"max_tokens":48,"temperature":0}`
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Post(serverURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var out struct {
		Timings struct {
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return 0
	}
	return out.Timings.PredictedPerSecond
}

// reportDecode prints the measured speeds against the 40-80 tok/s decode
// baseline band.
func reportDecode(pp, tps float64, ctx int) {
	if tps <= 0 {
		return
	}
	note := ""
	if pp > 0 {
		note = fmt.Sprintf(" (prompt ~%.0f tok/s)", pp)
	}
	switch {
	case tps < 40:
		ui.Warn("decode: ~%.0f tok/s%s at %d context - below the 40 tok/s baseline (a smaller model or quant would be faster; see 'winc detect')", tps, note, ctx)
	default:
		ui.Info("decode: ~%.0f tok/s%s at %d context", tps, note, ctx)
	}
}

// launchMemoPath is the per-model memo of the last measured-good launch (context +
// KV cache type), so subsequent launches load ONCE instead of re-walking the ladder
// (4+ failed loads of a 20 GB model is minutes of startup). Validated every start:
// a stale entry (hardware/engine changed) just falls back to the full ladder and is
// rewritten. Delete the file to force a re-measure.
func launchMemoPath() string { return filepath.Join(paths.InstallDir(), ".winc-launch") }

// launchMemoKey identifies a model for the launch memo: base name + size, so a
// re-downloaded or re-quantized file re-measures.
func launchMemoKey(modelPath string) string {
	return fmt.Sprintf("%s|%d", filepath.Base(modelPath), engine.FileMB(modelPath))
}

// launchFingerprint condenses every sizing-relevant launch input. The memo's
// remembered stepping is replayed only while ALL of them match -- a stepping
// measured under different inputs is a different launch, not a cache hit:
// context "optimal" vs "auto" change the target, a card appearing/vanishing
// changes the budget, team escalation's --parallel changes the slot split,
// and the KV/MoE/MTP knobs change what fits. Computed against the ORIGINAL
// config before the launch mutates it.
func launchFingerprint(cfg *config.Config, hw platform.Hardware) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s|%v|%s|%s|%s|%s|%d|%d|%v|%d",
		strings.ToLower(strings.TrimSpace(cfg.Performance.Context)),
		strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType)),
		cfg.Performance.FlashAttn,
		strings.ToLower(strings.TrimSpace(cfg.Performance.CpuMoe)),
		strings.ToLower(strings.TrimSpace(cfg.Performance.GpuLayers)),
		strings.ToLower(strings.TrimSpace(cfg.Performance.Mtp)),
		strings.TrimSpace(cfg.Performance.DraftModel),
		engine.ParallelSlots(cfg),
		hw.VRAMMB, hw.Unified, len(hw.GPUs),
	)
	return fmt.Sprintf("%08x", h.Sum32())
}

// loadLaunchMemo returns the memoized (ctx, cacheType, decode tok/s, placement)
// for a model under the given launch fingerprint, or (0, "", 0, plGPU). The
// placement tells the replay HOW the window loaded: plSpill must not force
// full-GPU residency (the gate would reject it every start), plNoMTP must keep
// the draft off (its allowance is what made the window fit). Entries written
// under a different fingerprint -- or by versions with older formats -- miss,
// re-measure once, and are rewritten in the current form.
func loadLaunchMemo(modelPath, fp string) (int, string, float64, string) {
	data, err := os.ReadFile(launchMemoPath())
	if err != nil {
		return 0, "", 0, plGPU
	}
	key := launchMemoKey(modelPath)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 6 && f[0] == key && f[4] == fp {
			if ctx, err := strconv.Atoi(f[1]); err == nil && ctx > 0 {
				tps, _ := strconv.ParseFloat(f[3], 64)
				pl := f[5]
				if pl != plSpill && pl != plNoMTP {
					pl = plGPU
				}
				return ctx, f[2], tps, pl
			}
		}
	}
	return 0, "", 0, plGPU
}

// saveLaunchMemo records a measured-good launch (its measured decode speed and
// placement) under its fingerprint. One line per (model, fingerprint): a model
// launched in several geometries (single vs team --parallel) keeps one
// remembered stepping per geometry instead of the modes evicting each other.
// Same-key lines from older formats can never match again and are purged.
func saveLaunchMemo(modelPath string, ctx int, cacheType string, tps float64, fp, placement string) {
	key := launchMemoKey(modelPath)
	var out []string
	if data, err := os.ReadFile(launchMemoPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			if f := strings.Fields(t); f[0] == key && (len(f) != 6 || f[4] == fp) {
				continue // replaced below (same fingerprint) or unreadable legacy format
			}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = append(out, "# winc: last measured-good launch per model (ctx + KV cache + decode tok/s + config fingerprint + placement). Delete to re-measure.")
	}
	out = append(out, fmt.Sprintf("%s %d %s %.1f %s %s", key, ctx, cacheType, tps, fp, placement))
	_ = os.WriteFile(launchMemoPath(), []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

// kvProbePath is the per-model memo of KV-cache upgrade probes, so a probe's
// failed-load cost is paid once per model, not on every launch.
func kvProbePath() string { return filepath.Join(paths.InstallDir(), ".winc-kvprobe") }

func loadKVProbe() map[string]int {
	m := map[string]int{}
	data, err := os.ReadFile(kvProbePath())
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.Atoi(f[1]); err == nil {
			m[f[0]] = v
		}
	}
	return m
}

func saveKVProbe(m map[string]int) {
	var b strings.Builder
	b.WriteString("# winc: measured q4 KV-cache upgrade results per model|q8ctx (0 = no gain). Delete to re-probe.\n")
	for k, v := range m {
		fmt.Fprintf(&b, "%s %d\n", k, v)
	}
	_ = os.WriteFile(kvProbePath(), []byte(b.String()), 0o644)
}

// upgradeLadderQ4 widens a window that settled below the sizing target. The formula
// can't see context-scaled overheads (the MTP draft's own cache, compute buffers),
// so the ladder sometimes lands short -- or leaves the per-slot window starved.
// Probe the NEXT standard rungs with q4_0 KV caches (main + MTP draft, half the
// bytes per token) and keep climbing while they load, keeping forced full-GPU
// placement throughout (the engine's spill-happy fit costs 2-4x decode; measured).
// Verified on a 16+12 GB pair: the 35B MoE goes 131072 -> 262144 fully on GPU at
// full speed. Outcomes are memoized per model so the probe cost is paid once.
func upgradeLadderQ4(cfg *config.Config, hw platform.Hardware, modelPath, bin string, port int, serverURL, logPath string, proc *server.Proc, ctx int, gated bool) (*server.Proc, int) {
	raw := strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType))
	if (raw != "" && raw != "auto") || !cfg.Performance.FlashAttn {
		return proc, ctx
	}
	mb := engine.FileMB(modelPath)
	off := engine.WillOffloadExperts(cfg, hw, modelPath)
	if engine.EffectiveCacheType(cfg, hw, modelPath, mb, off) != "q8_0" {
		return proc, ctx // the formula already picked q4; this IS the q4 result
	}
	target := engine.ResolveContext(cfg, hw, modelPath, mb, off)
	starved := ctx/engine.ParallelSlots(cfg) < engine.StarvedCtxTokens
	if ctx >= target && !starved {
		return proc, ctx // landed where the formula aimed, and no slot is starved
	}

	key := fmt.Sprintf("%s|%d", filepath.Base(modelPath), ctx)
	memo := loadKVProbe()
	if best, ok := memo[key]; ok {
		if best <= ctx {
			return proc, ctx // probed before: q4 gains nothing on this model here
		}
		ui.Info("KV cache upgrade (memoized): %d -> %d tokens...", ctx, best)
		proc.Stop()
		q4 := *cfg
		q4.Performance.CacheType = starvedKV
		if p2 := tryContextOnce(&q4, hw, modelPath, bin, port, serverURL, logPath, best, false, false, gated); p2 != nil {
			cfg.Performance.CacheType = starvedKV
			return p2, best
		}
		// Didn't come back (VRAM situation changed?) -- fall through to a fresh probe.
	}

	ui.Info("context settled at %d (target %d) - probing larger windows with the %s KV cache...", ctx, target, starvedKV)
	proc.Stop()
	q4 := *cfg
	q4.Performance.CacheType = starvedKV
	var bestProc *server.Proc
	best := ctx
	for {
		next := engine.NextLadderRung(best)
		if next <= best {
			break
		}
		if !rungWorthTrying(&q4, hw, modelPath, next, 1, false) {
			break
		}
		p2 := tryContextOnce(&q4, hw, modelPath, bin, port, serverURL, logPath, next, false, false, gated)
		if p2 == nil {
			break
		}
		if bestProc != nil {
			bestProc.Stop()
		}
		bestProc, best = p2, next
		if best >= target {
			break
		}
	}
	if bestProc != nil {
		memo[key] = best
		saveKVProbe(memo)
		cfg.Performance.CacheType = starvedKV
		ui.Good("%s KV cache widened the context: %d -> %d tokens", starvedKV, ctx, best)
		return bestProc, best
	}
	memo[key] = 0
	saveKVProbe(memo)
	// No gain -- reload the exact q8 setup that just worked. Last resort: this
	// rung already passed the placement gate moments ago, so a gate hiccup here
	// must warn, not fail the whole launch.
	if p2 := tryContextOnce(cfg, hw, modelPath, bin, port, serverURL, logPath, ctx, false, true, gated); p2 != nil {
		return p2, ctx
	}
	ui.Err("engine did not come back after the KV-cache probe; see %s", logPath)
	return nil, 0
}

func backendLabel(b string) string {
	if b == "" {
		return "installed engine"
	}
	return b + " backend"
}
