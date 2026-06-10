package cli

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/ui"
)

// autoPairDraft turns on speculative decoding automatically for a DENSE target that
// has a known same-tokenizer draft already downloaded -- unless the user set
// draft_model explicitly (their choice wins). It is per-model and safe: MoE models
// carry no draft mapping in the catalogue, so they are never paired (speculative
// decoding is net-negative on MoE). Sets cfg.Performance.DraftModel to the draft's
// filename; ServerArgs resolves it against the models dir and skips it if missing.
func autoPairDraft(cfg *config.Config, cat *catalog.Catalog, modelQuery string) {
	if strings.TrimSpace(cfg.Performance.DraftModel) != "" {
		return // explicit user override wins
	}
	m := cat.Find(modelQuery)
	draft := cat.DraftFor(m)
	if draft == nil {
		if m == nil {
			autoPairUncataloguedDraft(cfg, cat, modelQuery)
		}
		return
	}
	if !fileExists(filepath.Join(modelsDir(cfg), draft.LocalFile())) {
		ui.Dim("tip: 'winc -d %s' enables speculative decoding (faster) for %s", draft.Alias, m.Alias)
		return
	}
	cfg.Performance.DraftModel = draft.LocalFile()
	ui.Info("speculative decoding on: drafting %s with %s", m.Alias, draft.Alias)
}

// qwen35Family matches the Qwen3.5 model family in a filename -- the one family
// whose tokenizer the catalog's 0.8B draft shares (Qwen3.6 changed tokenizers and
// must never be externally drafted).
var qwen35Family = regexp.MustCompile(`(?i)qwen[-_.]?3[._]5`)

// autoPairUncataloguedDraft extends speculative decoding to unknown downloads with
// catalog-family features: a big dense Qwen3.5-family GGUF pairs with the catalog's
// 0.8B draft when both are downloaded. MoE files are skipped (drafts backfire on
// MoE), MTP files carry their own heads, and tiny models aren't worth drafting.
func autoPairUncataloguedDraft(cfg *config.Config, cat *catalog.Catalog, modelQuery string) {
	p, _ := downloadedPath(cfg, cat, modelQuery)
	if p == "" {
		return
	}
	base := filepath.Base(p)
	if !qwen35Family.MatchString(base) || engine.IsMoEFile(base) || engine.IsMTPFile(base) {
		return
	}
	if engine.FileMB(p) < 4000 {
		return // the draft only pays off on 9B-class and bigger
	}
	draft := cat.Find("qwen3.5-0.8b")
	if draft == nil || strings.EqualFold(base, draft.LocalFile()) {
		return
	}
	if !fileExists(filepath.Join(modelsDir(cfg), draft.LocalFile())) {
		ui.Dim("tip: 'winc -d %s' enables speculative decoding (faster) for %s (same family)", draft.Alias, base)
		return
	}
	cfg.Performance.DraftModel = draft.LocalFile()
	ui.Info("speculative decoding on: drafting %s with %s (family match)", base, draft.Alias)
}

// offerDraft prompts to also fetch the matching draft for a freshly-downloaded dense
// model, so speculative decoding turns on automatically on the next launch. autoYes
// downloads it without asking. No-op for MoE / models without a draft / drafts that
// are already present.
func offerDraft(cfg *config.Config, cat *catalog.Catalog, m *catalog.Model, autoYes bool) {
	draft := cat.DraftFor(m)
	if draft == nil {
		return
	}
	md := modelsDir(cfg)
	if fileExists(filepath.Join(md, draft.LocalFile())) {
		ui.Info("speculative decoding ready: %s will draft with %s", m.Alias, draft.Alias)
		return
	}
	q := fmt.Sprintf("%s is a dense model - also download its %s draft (%s) to enable speculative decoding (faster)?", m.Alias, draft.Alias, draft.Size)
	if !autoYes && !ui.Confirm(q, true) {
		ui.Dim("skipped - run 'winc -d %s' anytime to turn it on later", draft.Alias)
		return
	}
	ui.Good("Downloading draft %s", draft.File)
	ui.Say("  from %s", draft.Repo)
	if _, err := download.HFDownload(draft.Repo, draft.File, md, cfg.HuggingFace.Token); err != nil {
		ui.Warn("draft download failed: %v (speculative decoding stays off)", err)
		return
	}
	ui.Good("draft ready - speculative decoding turns on automatically for %s", m.Alias)
}

// offerMTPHead prompts to also fetch the small external MTP drafter head for a
// freshly-downloaded model that ships one (Gemma 4 keeps its prediction heads in
// a separate GGUF). Once the head sits next to the model, winc pairs it at launch
// automatically (--spec-type draft-mtp + --spec-draft-model). autoYes fetches it
// without asking. No-op for models without a head / heads already present.
func offerMTPHead(cfg *config.Config, m *catalog.Model, autoYes bool) {
	if m == nil || m.MtpHead == "" {
		return
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
