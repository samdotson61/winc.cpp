package cli

import (
	"fmt"
	"path/filepath"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/ui"
)

// External-draft speculation (the dense 0.8B auto-pair) is RETIRED as of v1.28.0:
// it measured as a decode LOSS on every backend it was ever tested on, across two
// engine generations -- CUDA 5070 Ti/b9672: 9B+0.8B draft-simple -43% code / -57%
// chat at temp 0 DESPITE 67% acceptance (the draft's serial generation time exceeds
// what batch verification saves at 119 tok/s native); Metal M4 Pro (v1.27.0): 0%
// best-case for an 827 MiB load; CPU/low-VRAM tiers (2026-06-13 eval-shape pass):
// decode halved at every tier. An explicit [performance].draft_model in winc.toml
// still works verbatim -- retirement only removes the automatic pairing and the
// download-time offers. MTP heads are a different, measured-positive mechanism on
// CUDA and keep their offer + auto-launch below.

// speculationUselessHere reports whether speculative decoding (MTP included)
// should be suppressed on this backend. On Metal it is net-negative: the backend
// doesn't get the batch-verification parallelism that drafting needs. MEASURED
// (M4 Pro, v1.27.0): MTP costs -8% (n=1) to -38% (n=3) decode vs off.
// engine.mtpActive already gates MTP off on Metal (originally for a crash); this
// extends the same call to the MTP-head download offer, so a Metal user is never
// nudged to fetch a head that can't help.
func speculationUselessHere() bool { return engine.CurrentBackend() == "metal" }

// offerMTPHead prompts to also fetch the small external MTP drafter head for a
// freshly-downloaded model that ships one (Gemma 4 keeps its prediction heads in
// a separate GGUF). Once the head sits next to the model, winc pairs it at launch
// automatically (--spec-type draft-mtp + --spec-draft-model). autoYes fetches it
// without asking. No-op for models without a head / heads already present.
func offerMTPHead(cfg *config.Config, m *catalog.Model, autoYes bool) {
	if m == nil || m.MtpHead == "" {
		return
	}
	if speculationUselessHere() {
		return // MTP runs off on Metal (see engine.mtpActive) -- the head can't help there
	}
	md := modelsDir(cfg)
	local := filepath.Base(m.MtpHead)
	if fileExists(filepath.Join(md, local)) {
		ui.Info("MTP ready: %s pairs with its drafter head automatically at launch", m.Alias)
		return
	}
	q := fmt.Sprintf("%s ships a small MTP drafter head - also download it to speed up decoding (~1.5x)?", m.Alias)
	if !autoYes && !ui.Confirm(q, true) {
		ui.Dim("skipped - run 'winc -d %s' anytime to fetch it", m.Alias)
		return
	}
	ui.Good("Downloading MTP head %s", local)
	ui.Say("  from %s", m.Repo)
	if _, err := download.HFDownloadAs(m.Repo, m.MtpHead, md, local, cfg.HuggingFace.Token); err != nil {
		ui.Warn("MTP head download failed: %v (the model runs fine without it)", err)
		return
	}
	ui.Good("MTP head ready - multi-token prediction turns on automatically for %s", m.Alias)
}
