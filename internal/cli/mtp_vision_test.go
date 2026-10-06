package cli

import (
	"os"
	"path/filepath"
	"testing"

	"winc/internal/config"
	"winc/internal/engine"
)

// The launch warning fires exactly when an MTP-capable model is about to start
// with a vision projector loaded -- and stays quiet when vision is off, when the
// model has no MTP, or on a no-MTP retry. Files are empty: MTP detection falls
// back to the filename (the pre-download rule) and projector pairing is by name.
func TestMTPYieldsToVisionGate(t *testing.T) {
	if engine.CurrentBackend() == "metal" {
		t.Skip("MTP never engages on Metal, so the warning never fires there")
	}
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mtp := touch("Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf")
	plain := touch("Qwen3.5-4B-Q4_K_M.gguf")
	cfg := config.Defaults()

	if mtpYieldsToVision(&cfg, mtp, false) {
		t.Fatal("no projector on disk: nothing to yield to")
	}
	touch("Qwen3.6-35B-mmproj.gguf")
	if !mtpYieldsToVision(&cfg, mtp, false) {
		t.Fatal("MTP model + its projector + vision auto: the warning must fire")
	}
	if mtpYieldsToVision(&cfg, mtp, true) {
		t.Fatal("a no-MTP retry launch must not warn (MTP is off on purpose)")
	}
	if mtpYieldsToVision(&cfg, plain, false) {
		t.Fatal("a model without MTP has nothing to lose")
	}
	off := cfg
	off.Performance.Vision = "off"
	if mtpYieldsToVision(&off, mtp, false) {
		t.Fatal(`vision = "off" is the documented fix: MTP engages, no warning`)
	}
	noMTP := cfg
	noMTP.Performance.Mtp = "off"
	if mtpYieldsToVision(&noMTP, mtp, false) {
		t.Fatal(`mtp = "off": the user chose it, nothing yields`)
	}
}
