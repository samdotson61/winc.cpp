package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/router"
	"winc/internal/ui"
)

// The jobdar evaluation profile (`winc serve --eval`, winc-jobdar branch).
//
// jobdar's eval calls are the OPPOSITE shape of an agent session: a 1-5k-token
// posting+resume prompt, a few-hundred-token JSON verdict, many independent
// requests. Every knob below is a MEASURED choice for that shape:
//
//   - context 16384, q8 KV: an eval never needs an agent window; the whole
//     server fits a 2 GB card with the 2B (~1.7 GB) and a 4 GB card with the
//     4B (~3.4 GB). 16384 holds on EVERY tier: a smaller pin is honored by
//     ResolveContext but the LAUNCH fitting path normalizes to its rungs --
//     measured live (1.23.0-jobdar.3, synthetic 2 GB box): an 8192 eval pin
//     launched at -c 16384. The original round-up note here was right.
//   - low-tier preset (applyEvalTier): a <4 GB budget pins ONE eval slot, a
//     4-8 GB budget pins TWO (vs the engine's default 4) -- concurrent evals
//     on a thermally-limited box contend for the unified KV pool (evicting
//     the prompt cache) and throttle sustained speed. REASONED defaults for
//     phone/tablet-class hardware -- slot count changes throughput/thermals,
//     never verdicts -- pending on-target sustained-batch measurement (bench
//     50-100 evals at steady state, not a burst: this class throttles after
//     2-5 minutes). Args verified live: --parallel 1 -c 16384, healthy, JSON
//     eval answered.
//   - reasoning OFF: thinking routes every generated token into the reasoning
//     channel; jobdar would receive EMPTY content with the budget spent.
//     With it off the same models answer in 91-172 tokens at full speed.
//   - speculative draft OFF: the 0.8B draft measured a ~50% decode LOSS at
//     this shape on every tier (it is 20-40% of the target models' size).
//   - no team mode, no MTP: single model, single purpose.
//   - the ROUTER binds the winc.toml port: jobdar's documented contract is
//     a STABLE Anthropic /v1/messages surface (inference_url) -- agent flows
//     get their router URL programmatically, jobdar configures it once.
//     llama-server itself moves to an ephemeral port behind it.
//
// Engine auto-parallel stays untouched on >=8 GB budgets: serve mode runs a
// UNIFIED KV pool (4 slots), so jobdar's scan-stage concurrency multiplies
// throughput without any per-slot window split. Smaller budgets pin fewer
// slots (applyEvalTier).
const evalCtxTokens = 16384

// Low-tier preset thresholds (applyEvalTier). Budgets are MemoryBudgetMB:
// dedicated VRAM on dGPU boxes, ~72% of RAM on unified, RAM-scaled CPU-only.
const (
	evalTinyBudgetMB  = 4096 // below: 1 eval slot (2 GB-card class)
	evalSmallBudgetMB = 8192 // below: 2 eval slots
)

// applyEvalTier pins the hardware-tiered eval knobs; applyEvalProfile holds the
// hardware-independent ones. Slots ONLY -- a smaller window was tried and
// measured to round up through the launch ladder (see the profile comment).
// See there too for the validation status (reasoned defaults, throughput-only,
// never verdict-affecting). Unknown hardware (budget 0) changes nothing --
// engine defaults, no guess.
func applyEvalTier(cfg *config.Config, hw platform.Hardware) {
	switch budget := hw.MemoryBudgetMB(); {
	case budget <= 0:
	case budget < evalTinyBudgetMB:
		cfg.Performance.EvalSlots = 1
	case budget < evalSmallBudgetMB:
		cfg.Performance.EvalSlots = 2
	}
}

// applyEvalProfile pins the measured eval-profile knobs onto cfg.
//
// NOTE (nano-sweep, 2026-06-14; SHIPPED in 1.21.4-jobdar.4): deterministic scoring
// needs BOTH levers, and neither works alone -- temp-0 on /v1/messages alone WORSENS
// parse-fails (12/24, the model deterministically derails out of JSON), and JSON-alone
// at the agent temp 0.7 is only 79% with 2 dangerous accepts. Measured together on
// qwen3.5-2b (1.6 GiB): temp 0 + guaranteed-JSON (response_format=json_schema on
// /v1/chat/completions) → 100% acc / 0 parse-fails / 0 dangerous accepts, where the
// inherited agent sampling swung 65%→100% across runs. Both halves are now live:
// GreedySampling below pins argmax here, and jobdar routes evals through the
// JSON-schema endpoint (its side of the coordinated change).
func applyEvalProfile(cfg *config.Config) {
	cfg.Performance.Context = strconv.Itoa(evalCtxTokens)
	cfg.Performance.CacheType = "q8_0"
	cfg.Performance.Mtp = "off"
	cfg.Performance.DraftModel = ""
	cfg.Reasoning.Mode = "off"
	cfg.Performance.GreedySampling = true // deterministic scoring: argmax, not agent sampling
}

// evalEvalThresholdMB is the VRAM at/above which the eval profile prefers the
// Qwen 4B over the low-end default. The 4B is the measured eval anchor (12/12 on
// the 12-posting policy-boundary set) and at the 16384 eval window it occupies a
// MEASURED 3.3 GB fully resident -- so a 5 GB-class card hosts it with ~1 GB to
// spare. Set to 5 GB so the extra GB of cards get it; below it the low-end
// default leads, the right call for 4 GB laptops where the 4B's 3.3 GB is too
// tight against desktop overhead.
const evalEvalThresholdMB = 5120

// evalPickModel chooses the measured-best eval model this hardware affords, by
// tier-ordered preference (first downloaded wins).
//
// LOW END (< threshold): gemma4-e2b leads. Head-to-head on the 12-posting policy
// set (identical conditions), gemma4-e2b scored 12/12 -- the ONLY sub-3 GB model
// that rejects every senior/mid/manager trap -- at 108 tok/s, beating the Qwen
// 2B-Q4 (10/12: it over-ACCEPTS senior and manager roles as entry, the dangerous
// failure). The eval profile runs the draft OFF, so gemma's different model family
// costs nothing here (there is no shared speculative draft to keep).
//
//	NOTE (nano-sweep re-measure, 2026-06-14): ~1.75 GB is e2b's WEIGHTS, not its
//	footprint -- at the 16384 eval window its RESIDENT memory is ~3 GiB, ≈ the 4B's
//	(the Matformer stores ~4B weights, activates ~2B). So e2b's real edge over the
//	4B is ~2x SPEED at tied accuracy, NOT "half the VRAM"; resident footprints are
//	~equal. The reason to flip to the 4B at 5 GB+ is headroom/quality ceiling, not
//	memory savings from e2b.
//
// Qwen 2B-Q4 is the fallback; qwen3.5-2b-q8 is deliberately absent -- it is bigger
// and slower than the Q4. (The nano-sweep re-measure found 2B-Q8 MORE accurate than
// 2B-Q4, 89.5% vs 65% on the 8-JD set, so it is excluded on footprint/speed, NOT on
// accuracy: a manual fidelity option.)
//
// 5 GB+: the Qwen 4B anchor leads (also 12/12, the quality ceiling here), then
// gemma4-e2b, then the 2B.
//
// RE-CHECKED 2026-07-21 on the current decomposed Jobfaro rubric (6-posting
// harness, 5070 Ti): the first pass served WITHOUT the production stack (no
// schema grammar, plain sampling) and measured the 2B-Q4 at 1/6 valid JSON —
// which turned out to be a HARNESS ARTIFACT. Re-measured the same day under
// the REAL production shape (this profile's --reasoning off + --temp 0
// --top-k 1, PLUS jobfaro's response_format=json_schema and max_tokens 700):
// the 2B is 6/6 valid JSON, 5/6 correct — including BOTH senior/mid reject
// traps — answering in 219-342 tokens; the same server without the schema
// drops to 3/6 JSON. Mechanism: schema-less serving lets Qwen3.5 verbalize
// reasoning into the content (the documented reasoning-leak class); the
// grammar is LOAD-BEARING for the 2B. gemma4-e2b measured 5/6 + 6/6 in both
// shapes — it needs no grammar to behave, which is exactly why it leads this
// tier: its June 12-posting H2H edge (12/12 vs the 2B's 10/12, REJECT 5/5 vs
// 3/5) plus shape-robustness. The 2B is a fully-functional fallback WHEN
// served through the production stack — not broken, just grammar-dependent.
// evalPrefs is the eval-model preference order for this hardware (first downloaded
// wins). The order flips at evalEvalThresholdMB per the note above: low-end leads
// with gemma4-e2b, 5 GB+ leads with the Qwen 4B anchor.
func evalPrefs(hw platform.Hardware) []string {
	if hw.VRAMMB >= evalEvalThresholdMB {
		return []string{"qwen3.5-4b", "gemma4-e2b", "qwen3.5-2b"}
	}
	return []string{"gemma4-e2b", "qwen3.5-2b", "qwen3.5-4b"}
}

// evalArmCPUPrefs: on an arm64 CPU-only install, each preference expands to try
// its -q40 ARM rung FIRST (Q4_0 is the format llama.cpp runtime-repacks to
// dotprod/i8mm layouts -- the prompt-speed format on ARM CPUs). First-downloaded
// still wins, so a rung engages only when the user deliberately pulled it, and
// the recommended DOWNLOAD (promptDownloadEvalModel) stays the policy-set-
// validated K-quant -- winc never auto-fetches the speed-first quant.
func evalArmCPUPrefs(hw platform.Hardware, prefs []string) []string {
	if hw.Arch != "arm64" || engine.CurrentBackend() != "cpu" {
		return prefs
	}
	out := make([]string, 0, len(prefs)*2)
	for _, a := range prefs {
		out = append(out, a+"-q40", a)
	}
	return out
}

func evalPickModel(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware) (path, alias string) {
	prefs := evalArmCPUPrefs(hw, evalPrefs(hw))
	for i, alias := range prefs {
		if p, a := downloadedPath(cfg, cat, alias); p != "" {
			// Recommend the tier's measured-accuracy leader whenever the pick fell
			// back past it. Recommend the BASE alias, never an arm -q40 rung (the
			// recommended download stays the policy-set-validated K-quant), and stay
			// quiet when the fallback is just the leader's own rung expansion.
			if best := evalPrefs(hw)[0]; i > 0 && strings.TrimSuffix(a, "-q40") != best {
				ui.Dim("preferred eval model %s isn't downloaded - using %s", best, a)
				ui.Info("for the most accurate evals on this hardware: winc -d %s (measured-accuracy leader for this tier)", best)
			}
			return p, a
		}
	}
	return "", "" // none downloaded — caller offers the recommended-download prompt
}

// promptDownloadEvalModel offers to fetch the recommended eval model for this
// hardware when none is downloaded — the SAME confirm-and-download prompt
// `winc setup` shows (recommendModel → ui.Confirm → HFDownload), scoped to the
// eval preference order so a fresh `winc serve --eval` bootstraps itself instead
// of erroring out. Returns ("","") if the model is unknown, the user declines, or
// the download fails.
func promptDownloadEvalModel(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware) (path, alias string) {
	want := evalPrefs(hw)[0]
	m := cat.Find(want)
	if m == nil {
		ui.Err("no eval model downloaded")
		ui.Info("get one with: winc -d %s", want)
		return "", ""
	}
	tier := catalog.VramTier(hw.MemoryBudgetMB())
	if !ui.Confirm(fmt.Sprintf("Download recommended model %s (%s) for tier '%s'?", m.Alias, m.Size, tier), true) {
		ui.Info("get one later with: winc -d %s", want)
		return "", ""
	}
	saveAs := m.File
	if m.Save != "" {
		saveAs = m.Save
	}
	if _, err := download.HFDownloadAs(m.Repo, m.File, modelsDir(cfg), saveAs, cfg.HuggingFace.Token); err != nil {
		ui.Err("model download failed: %v", err)
		return "", ""
	}
	ui.Good("downloaded %s", m.Alias)
	return downloadedPath(cfg, cat, want)
}

// cmdServeEval runs the eval profile until Ctrl-C.
func cmdServeEval(cfg *config.Config, cat *catalog.Catalog, pos []string) int {
	applyEvalProfile(cfg)
	hw := platform.DetectHardwareCached()
	applyEvalTier(cfg, hw)

	var modelPath, alias string
	if len(pos) >= 1 {
		modelPath, alias = downloadedPath(cfg, cat, pos[0])
		if modelPath == "" {
			reportMissingModel(alias, pos[0])
			return 1
		}
	} else if modelPath, alias = evalPickModel(cfg, cat, hw); modelPath == "" {
		// No eval model downloaded — offer to fetch the recommended one, the same
		// confirm-and-download prompt `winc setup` uses, so a fresh `winc serve
		// --eval` is self-bootstrapping instead of erroring out.
		if modelPath, alias = promptDownloadEvalModel(cfg, cat, hw); modelPath == "" {
			return 1
		}
	}

	// Eval models are small (<= ~5 GB) and always fit ONE card; splitting one
	// across devices gains nothing and some architectures (gemma4) ABORT under
	// the engine's multi-device scheduler (GGML_SCHED_MAX_SPLIT_INPUTS) -- the
	// CUDA backend also enumerates each card as a Vulkan device, so an unpinned
	// -ngl 99 tries to spread the model across CUDA0 + Vulkan0 + Vulkan1 and
	// blows the split-input limit. Pin to the single biggest GPU with
	// `-sm none -mg N` (split-mode none = main device only, backend-agnostic,
	// the same lever winc's own GPU-speed probe uses) -- correct AND faster (no
	// cross-PCIe). The model was already chosen against the FULL VRAM budget
	// above; only the placement narrows here.
	if len(hw.GPUs) > 1 {
		best := 0
		for i := range hw.GPUs {
			if hw.GPUs[i].TotalMB > hw.GPUs[best].TotalMB {
				best = i
			}
		}
		ui.Dim("pinning the eval load to GPU %d (%s) - small models don't benefit from a multi-GPU split", best, hw.GPUs[best].Name)
		cfg.Performance.ExtraServerArgs = append(cfg.Performance.ExtraServerArgs, "-sm", "none", "-mg", strconv.Itoa(best))
		hw.GPUs = []platform.GPUDevice{hw.GPUs[best]}
		hw.VRAMMB = hw.GPUs[0].TotalMB
	}

	llamaPort := freePort()
	if llamaPort == 0 {
		ui.Err("no free local port for the engine")
		return 1
	}
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", llamaPort)
	logPath := filepath.Join(paths.InstallDir(), "llama-server.log")
	ui.Good("eval profile: %s (%s)", alias, filepath.Base(modelPath))
	// Print the EFFECTIVE config, not the profile constants -- the low-tier
	// preset may have adjusted it, and status text must reflect what actually
	// runs (the const was printed here while a tier pin ran underneath).
	ui.Info("window %s - reasoning off - speculative draft off - q8 KV", cfg.Performance.Context)
	if cfg.Performance.EvalSlots > 0 {
		ui.Info("low-tier preset: %d eval slot(s) for this memory budget (engine default is 4)", cfg.Performance.EvalSlots)
	}
	proc, loadedCtx := startLlamaFitting(cfg, hw, modelPath, llamaPort, serverURL, logPath)
	if proc == nil {
		ui.Err("could not start the engine; see %s", logPath)
		return 1
	}
	defer proc.Stop()

	// The stable jobdar-facing surface: the router on the winc.toml port.
	addr := fmt.Sprintf("%s:%d", cfg.General.Host, cfg.General.Port)
	r, rerr := router.Start(cfg, serverURL, loadedCtx, addr)
	if rerr != nil {
		ui.Err("could not bind %s: %v", addr, rerr)
		ui.Info("another server owns that port - stop it, or change [general] port in winc.toml and set jobdar's inference_url to match")
		return 1
	}
	defer r.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	ui.Good("ready - jobdar inference_url: %s  (Anthropic /v1/messages)", r.BaseURL())
	ui.Info("model %s at %d-token window - Ctrl-C to stop", alias, loadedCtx)
	// Structured-output path: jobdar can guarantee valid eval JSON by sending
	// response_format=json_schema to the OpenAI-compatible endpoint (verified to
	// pass through the router to the engine). Other calls stay on /v1/messages.
	ui.Dim("guaranteed-JSON evals: POST %s/v1/chat/completions with response_format=json_schema", r.BaseURL())
	<-sig
	ui.Say("stopping...")
	return 0
}
