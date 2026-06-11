package cli

import (
	"testing"

	"winc/internal/catalog"
)

// The recommendation is CONSERVATIVE at the low end: the pick must leave
// model-scaled runtime headroom within the budget (~1 GB for nano models --
// measured on the 4B -- up to the 2 GB calibrated on mid-tier), stepping down
// a tier when the budget tier's models can't honestly host a context. A 4 GB
// card gets the 4B (a capable coder it demonstrably runs), and a 12 GB card
// gets a small-tier model that runs well instead of a 13.6 GB mid-tier
// flagship with no room left.
func TestRecommendModelConservative(t *testing.T) {
	cat := catalog.Load(nil)
	cases := map[int]string{
		4096:  "qwen3.5-4b",     // 4 GB: 2.6 GB weights + ~1.3 GB measured runtime fits
		5800:  "qwen3.5-4b",     // ~6 GB-class: nano default fits
		8192:  "qwen3.5-9b",     // 8 GB: small default stays
		12288: "qwen3.5-9b",     // 12 GB: mid tier has nothing honest -> descend to small
		16303: "qwen3.6-35b",    // 16 GB: mid default
		28591: "qwen3.6-35b-q4", // 28 GB dual-GPU: large default
	}
	for budget, want := range cases {
		got := recommendModel(cat, budget)
		if got == nil || got.Alias != want {
			alias := "(nil)"
			if got != nil {
				alias = got.Alias
			}
			t.Errorf("budget %d: recommended %s, want %s", budget, alias, want)
		}
	}
	// Unknown budget falls back to the tier default rather than nothing.
	if got := recommendModel(cat, 0); got == nil {
		t.Error("zero budget should still recommend something")
	}
}
