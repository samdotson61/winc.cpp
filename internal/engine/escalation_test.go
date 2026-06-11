package engine

import (
	"os"
	"path/filepath"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

func TestMainEscalationOK(t *testing.T) {
	cfg := config.Defaults()
	dir := t.TempDir()
	model := filepath.Join(dir, "main.gguf") // dense (non-MoE) name -> no expert offload
	if err := os.WriteFile(model, make([]byte, 10*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ample VRAM after the (tiny) model + a big window -> escalation to main is allowed.
	if !MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 24000}, model, 131072) {
		t.Error("ample VRAM should allow main escalation")
	}
	// Tight VRAM (below the headroom threshold) -> escalation capped at the CPU
	// worker. The stand-in model is tiny, so its scaled reserve is small too --
	// the fixture must be genuinely tight.
	if MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 6500}, model, 131072) {
		t.Error("tight VRAM should block main escalation")
	}
	// No GPU -> never escalate to main.
	if MainEscalationOK(&cfg, platform.Hardware{VRAMMB: 0}, model, 131072) {
		t.Error("no GPU should block main escalation")
	}
	// A window whose --parallel 2 half would be below the workable floor -> never
	// split (Claude Code's fixed overhead alone is ~24k; a 32k slot starves).
	if MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 24000}, model, 65536) {
		t.Error("a 65536 window must not be split into 32k slots")
	}
	// Unknown expected window (0) skips the window check; the post-launch guard covers it.
	if !MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 24000}, model, 0) {
		t.Error("unknown window should defer to the post-launch guard, not block")
	}
}
