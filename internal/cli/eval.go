package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
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
//     4B (~3.4 GB). 16384 is the context ladder's floor rung -- a smaller pin
//     would silently round up to it anyway.
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
// Engine auto-parallel stays untouched: serve mode runs a UNIFIED KV pool
// (4 slots), so jobdar's scan-stage concurrency multiplies throughput without
// any per-slot window split.
const evalCtxTokens = 16384

// applyEvalProfile pins the measured eval-profile knobs onto cfg.
//
// NOTE (nano-sweep, 2026-06-14): the profile still inherits each model's AGENT
// sampling via FamilySamplingArgs (Qwen temp 0.7 / Gemma temp 1.0). For deterministic
// SCORING that injects real band noise -- a 2B swings 65%→100% accuracy across runs at
// temp 0.7. Measured low-end win on qwen3.5-2b (1.6 GiB, half the e2b footprint):
// temp 0 + guaranteed-JSON (response_format=json_schema on /v1/chat/completions) →
// 100% acc / 0 parse-fails / 0 dangerous accepts. The levers DON'T work alone: temp-0
// on /v1/messages WORSENS parse-fails (12/24, the model deterministically derails out
// of JSON), and JSON-alone at temp 0.7 is only 79% with 2 dangerous accepts. Shipping
// this needs BOTH a temp-0 pin here AND jobdar routing evals through the JSON-schema
// endpoint -- deferred pending that coordinated change.
func applyEvalProfile(cfg *config.Config) {
	cfg.Performance.Context = strconv.Itoa(evalCtxTokens)
	cfg.Performance.CacheType = "q8_0"
	cfg.Performance.Mtp = "off"
	cfg.Performance.DraftModel = ""
	cfg.Reasoning.Mode = "off"
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
//   NOTE (nano-sweep re-measure, 2026-06-14): ~1.75 GB is e2b's WEIGHTS, not its
//   footprint -- at the 16384 eval window its RESIDENT memory is ~3 GiB, ≈ the 4B's
//   (the Matformer stores ~4B weights, activates ~2B). So e2b's real edge over the
//   4B is ~2x SPEED at tied accuracy, NOT "half the VRAM"; resident footprints are
//   ~equal. The reason to flip to the 4B at 5 GB+ is headroom/quality ceiling, not
//   memory savings from e2b.
// Qwen 2B-Q4 is the fallback; qwen3.5-2b-q8 is deliberately absent -- it is bigger
// and slower than the Q4. (The nano-sweep re-measure found 2B-Q8 MORE accurate than
// 2B-Q4, 89.5% vs 65% on the 8-JD set, so it is excluded on footprint/speed, NOT on
// accuracy: a manual fidelity option.)
//
// 5 GB+: the Qwen 4B anchor leads (also 12/12, the quality ceiling here), then
// gemma4-e2b, then the 2B.
// evalPrefs is the eval-model preference order for this hardware (first downloaded
// wins). The order flips at evalEvalThresholdMB per the note above: low-end leads
// with gemma4-e2b, 5 GB+ leads with the Qwen 4B anchor.
func evalPrefs(hw platform.Hardware) []string {
	if hw.VRAMMB >= evalEvalThresholdMB {
		return []string{"qwen3.5-4b", "gemma4-e2b", "qwen3.5-2b"}
	}
	return []string{"gemma4-e2b", "qwen3.5-2b", "qwen3.5-4b"}
}

func evalPickModel(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware) (path, alias string) {
	prefs := evalPrefs(hw)
	for i, alias := range prefs {
		if p, a := downloadedPath(cfg, cat, alias); p != "" {
			if i > 0 {
				ui.Dim("preferred eval model %s isn't downloaded - using %s", prefs[0], a)
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
	ui.Info("window %d - reasoning off - speculative draft off - q8 KV", evalCtxTokens)
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
