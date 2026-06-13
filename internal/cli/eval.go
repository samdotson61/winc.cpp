package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"

	"winc/internal/catalog"
	"winc/internal/config"
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
func applyEvalProfile(cfg *config.Config) {
	cfg.Performance.Context = strconv.Itoa(evalCtxTokens)
	cfg.Performance.CacheType = "q8_0"
	cfg.Performance.Mtp = "off"
	cfg.Performance.DraftModel = ""
	cfg.Reasoning.Mode = "off"
}

// evalEvalThresholdMB is the VRAM at/above which the eval profile prefers the
// 4B over the 2B. The 4B is the measured eval anchor (6/6 on the policy-boundary
// set incl. both the senior and mid-level rejection traps, vs the 2B's 5/6) and
// at the 16384 eval window it occupies a MEASURED 3.3 GB fully resident -- so a
// 5 GB-class card hosts it with ~1 GB to spare. Set to 5 GB (not 6): the extra
// GB of cards now get the better judge. Below it the 2B-Q4 stays (143 tok/s on a
// 12 GB-class card, ~40 on a desktop CPU; ~1.6 GB resident) -- the right call for
// 4 GB laptops where the 4B's 3.3 GB is too tight against desktop overhead.
const evalEvalThresholdMB = 5120

// evalPickModel chooses the measured-best eval model this hardware affords.
// Prefers the downloaded one; falls back to the other; else prints the exact
// download command. (Deliberately ignores qwen3.5-2b-q8: benchmarked SLOWER and
// LESS accurate than the 2B-Q4 for evals -- it is a manual-only fidelity option.)
func evalPickModel(cfg *config.Config, cat *catalog.Catalog, hw platform.Hardware) (path, alias string) {
	prefer, alt := "qwen3.5-2b", "qwen3.5-4b"
	if hw.VRAMMB >= evalEvalThresholdMB {
		prefer, alt = alt, prefer
	}
	if p, a := downloadedPath(cfg, cat, prefer); p != "" {
		return p, a
	}
	if p, a := downloadedPath(cfg, cat, alt); p != "" {
		ui.Dim("preferred eval model %s isn't downloaded - using %s", prefer, a)
		return p, a
	}
	ui.Err("no eval model downloaded")
	ui.Info("get one with: winc -d %s   (or: winc -d %s)", prefer, alt)
	return "", ""
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
		return 1
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
	<-sig
	ui.Say("stopping...")
	return 0
}
