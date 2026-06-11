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

	// Ample VRAM after the (tiny) model -> escalation to main is allowed (the
	// unified KV pool means no window split; concurrency costs headroom only).
	if !MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 24000}, model) {
		t.Error("ample VRAM should allow main escalation")
	}
	// Tight VRAM (below the headroom threshold) -> escalation capped at the CPU
	// worker. The stand-in model is tiny, so its scaled reserve is small too --
	// the fixture must be genuinely tight.
	if MainEscalationOK(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 6500}, model) {
		t.Error("tight VRAM should block main escalation")
	}
	// No GPU -> never escalate to main.
	if MainEscalationOK(&cfg, platform.Hardware{VRAMMB: 0}, model) {
		t.Error("no GPU should block main escalation")
	}
}
