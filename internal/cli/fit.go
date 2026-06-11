package cli

import (
	"encoding/json"
	"fmt"
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
// when it doesn't come up (the caller decides what to try next).
func tryContextOnce(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string, ctx int, noMTP bool) *server.Proc {
	waitVRAMFree(engine.FileMB(modelPath)+2048, 20*time.Second)
	args := engine.ServerArgs(cfg, hw, modelPath, port, "", ctx)
	if !noMTP {
		args = append(args, engine.MTPArgs(cfg, hw, modelPath, serverBin)...) // MTP variant -> --spec-type draft-mtp (if supported)
	}
	args = append(args, engine.CacheReuseArgs(serverBin)...) // extend prompt-cache reuse (probed)
	proc, err := server.Start(serverBin, args, logPath)
	if err != nil {
		return nil
	}
	if server.WaitReady(serverURL, "/health", 240*time.Second, proc.Dead) {
		return proc
	}
	proc.Stop() // didn't fit / failed
	return nil
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
func tryContextLadder(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string, noMTP bool) (*server.Proc, int) {
	target := engine.ResolveContext(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
	rungs := engine.ContextLadder(target)
	for i, ctx := range rungs {
		if !rungWorthTrying(cfg, hw, modelPath, ctx, 2, i == len(rungs)-1) {
			continue
		}
		if proc := tryContextOnce(cfg, hw, modelPath, serverBin, port, serverURL, logPath, ctx, noMTP); proc != nil {
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
		// Fast path: a previous session already measured this model's best window and
		// KV cache -- load straight there (one load instead of re-walking the ladder).
		if wasAuto {
			if mctx, mct, mtps := loadLaunchMemo(modelPath); mctx > 0 {
				lc := *cfg
				lc.Performance.CacheType = mct
				if proc := tryContextOnce(&lc, hw, modelPath, bin, port, serverURL, logPath, mctx, false); proc != nil {
					cfg.Performance.CacheType = mct
					// The decode bench is a real completion (seconds of startup); the speed
					// for this exact window + cache was measured when the memo was written,
					// so reuse it and re-measure only entries that predate the speed field.
					if mtps <= 0 {
						mtps = benchDecodeTPS(serverURL)
						saveLaunchMemo(modelPath, mctx, mct, mtps)
					}
					reportDecode(mtps, mctx)
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
				if proc, ctx := tryContextLadder(&lc2, hw, modelPath, bin, port, serverURL, logPath, false); proc != nil {
					lc2.Performance.Context = cfg.Performance.Context // restore auto for the probe's sizing math
					proc, ctx = upgradeLadderQ4(&lc2, hw, modelPath, bin, port, serverURL, logPath, proc, ctx)
					if proc != nil {
						cfg.Performance.CacheType = lc2.Performance.CacheType
						tps := benchDecodeTPS(serverURL)
						reportDecode(tps, ctx)
						ct := engine.EffectiveCacheType(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
						saveLaunchMemo(modelPath, ctx, ct, tps)
					}
					return proc, ctx
				}
				// Even the remembered range failed (hardware changed?) -> full flow below.
			}
		}
		if proc, ctx := tryContextLadder(cfg, hw, modelPath, bin, port, serverURL, logPath, false); proc != nil {
			proc, ctx = upgradeLadderQ4(cfg, hw, modelPath, bin, port, serverURL, logPath, proc, ctx)
			if proc != nil {
				tps := benchDecodeTPS(serverURL)
				reportDecode(tps, ctx)
				if wasAuto {
					ct := engine.EffectiveCacheType(cfg, hw, modelPath, engine.FileMB(modelPath), engine.WillOffloadExperts(cfg, hw, modelPath))
					saveLaunchMemo(modelPath, ctx, ct, tps)
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
			if proc, ctx := tryContextLadder(cfg, hw, modelPath, bin, port, serverURL, logPath, true); proc != nil {
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

// benchDecodeTPS measures real decode speed with a tiny fixed completion against
// the freshly-loaded (and warmed-up) server. Returns 0 when the request fails --
// the launch proceeds either way; this is measurement, never a gate.
func benchDecodeTPS(serverURL string) float64 {
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

// reportDecode prints the measured speed against the 40-80 tok/s baseline band.
func reportDecode(tps float64, ctx int) {
	if tps <= 0 {
		return
	}
	switch {
	case tps < 40:
		ui.Warn("decode: ~%.0f tok/s at %d context - below the 40 tok/s baseline (a smaller model or quant would be faster; see 'winc detect')", tps, ctx)
	default:
		ui.Info("decode: ~%.0f tok/s at %d context", tps, ctx)
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

// loadLaunchMemo returns the memoized (ctx, cacheType, decode tok/s) for a model,
// or (0, "", 0). tps is 0 for entries written before the speed field existed --
// the caller then measures once and rewrites the entry.
func loadLaunchMemo(modelPath string) (int, string, float64) {
	data, err := os.ReadFile(launchMemoPath())
	if err != nil {
		return 0, "", 0
	}
	key := launchMemoKey(modelPath)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if (len(f) == 3 || len(f) == 4) && f[0] == key {
			if ctx, err := strconv.Atoi(f[1]); err == nil && ctx > 0 {
				tps := 0.0
				if len(f) == 4 {
					tps, _ = strconv.ParseFloat(f[3], 64)
				}
				return ctx, f[2], tps
			}
		}
	}
	return 0, "", 0
}

// saveLaunchMemo records a measured-good launch (and its measured decode speed),
// replacing the model's prior line.
func saveLaunchMemo(modelPath string, ctx int, cacheType string, tps float64) {
	key := launchMemoKey(modelPath)
	var out []string
	if data, err := os.ReadFile(launchMemoPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, key+" ") {
				continue
			}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = append(out, "# winc: last measured-good launch per model (ctx + KV cache + decode tok/s). Delete to re-measure.")
	}
	out = append(out, fmt.Sprintf("%s %d %s %.1f", key, ctx, cacheType, tps))
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
func upgradeLadderQ4(cfg *config.Config, hw platform.Hardware, modelPath, bin string, port int, serverURL, logPath string, proc *server.Proc, ctx int) (*server.Proc, int) {
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
		if p2 := tryContextOnce(&q4, hw, modelPath, bin, port, serverURL, logPath, best, false); p2 != nil {
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
		p2 := tryContextOnce(&q4, hw, modelPath, bin, port, serverURL, logPath, next, false)
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
	// No gain -- reload the exact q8 setup that just worked.
	if p2 := tryContextOnce(cfg, hw, modelPath, bin, port, serverURL, logPath, ctx, false); p2 != nil {
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
