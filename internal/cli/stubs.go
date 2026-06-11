package cli

import (
	"os"
	"strings"

	"winc/internal/catalog"
	"winc/internal/config"
)

// shared helpers for setup/uninstall.

func anyModelDownloaded(cfg *config.Config) bool {
	entries, _ := os.ReadDir(modelsDir(cfg))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			return true
		}
	}
	return false
}

func firstInTier(cat *catalog.Catalog, tier string) *catalog.Model {
	if ms := cat.ByTier(tier); len(ms) > 0 {
		return &ms[0]
	}
	return nil
}

// recommendModel picks the default model for a memory budget CONSERVATIVELY: in
// the budget's tier -- then each tier below it -- the first model whose weights
// leave runtime headroom (compute buffers + a minimal KV cache). A low-end
// machine near a tier boundary thus steps DOWN to something that actually runs
// well, instead of getting the tier's flagship with no room for context. The
// budget tier's first entry is the last resort (budget unknown / nothing fits).
func recommendModel(cat *catalog.Catalog, budgetMB int) *catalog.Model {
	tier := catalog.VramTier(budgetMB)
	if budgetMB <= 0 {
		return firstInTier(cat, tier)
	}
	order := []string{"xl", "large", "mid", "small", "nano"}
	start := 0
	for i, t := range order {
		if t == tier {
			start = i
			break
		}
	}
	for _, t := range order[start:] {
		for _, m := range cat.ByTier(t) {
			mb := sizeStrToMB(m.Size)
			if mb+runtimeHeadroomMB(mb) <= budgetMB {
				mm := m
				return &mm
			}
		}
	}
	return firstInTier(cat, tier)
}

// runtimeHeadroomMB is what a model needs on top of its weights to run with a
// workable context. The old flat 2 GB was calibrated on mid-tier models (which
// keep it); measured on the 4B-Q4 (2.8 GB weights, full GPU): ~0.5 GB of CUDA +
// compute overhead plus ~0.5 GB of KV -- about 1 GB. The flat rule made a 4 GB
// card skip the 4B, a capable coder it demonstrably runs.
func runtimeHeadroomMB(modelMB int) int {
	h := modelMB / 2
	if h < 1024 {
		h = 1024
	}
	if h > 2048 {
		h = 2048
	}
	return h
}
