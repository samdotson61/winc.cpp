package cli

import (
	"winc/internal/catalog"
	"winc/internal/ui"
)

// mtpTip nudges toward the faster Multi-Token-Prediction variant of a just-downloaded
// standard model, if the catalogue lists one. No-op otherwise.
func mtpTip(cat *catalog.Catalog, m *catalog.Model) {
	if m == nil || m.Mtp == "" {
		return
	}
	if v := cat.Find(m.Mtp); v != nil {
		ui.Dim("tip: '%s' is a faster MTP variant (built-in multi-token prediction) - 'winc -d %s'", v.Alias, v.Alias)
	}
}
