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
