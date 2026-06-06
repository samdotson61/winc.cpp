package engine

import (
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

func TestServerArgsAdaptive(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "model.gguf", 8080, "", 0), " ")
	for _, want := range []string{"-m model.gguf", "--jinja", "-ngl 99", "-c 32768", "--port 8080", "--reasoning auto", "--flash-attn on"} {
		if !strings.Contains(s, want) {
			t.Errorf("args missing %q: %s", want, s)
		}
	}
}

func TestServerArgsExplicitCtx(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 65536), " ")
	if !strings.Contains(s, "-c 65536") {
		t.Errorf("explicit ctx not honored: %s", s)
	}
}

func TestServerArgsCPUNoOffload(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "linux", GPUVendor: "none"}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 9000, "", 0), " ")
	if !strings.Contains(s, "-ngl 0") {
		t.Errorf("cpu-only should use -ngl 0: %s", s)
	}
	if strings.Contains(s, "--flash-attn") {
		t.Errorf("flash-attn should be off without GPU offload: %s", s)
	}
}

func TestServerArgsReasoningModes(t *testing.T) {
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	for mode, want := range map[string]string{
		"off":   "--reasoning-budget 0",
		"on":    "--reasoning on",
		"fixed": "--reasoning-budget 2048",
	} {
		cfg := config.Defaults()
		cfg.Reasoning.Mode = mode
		s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 0), " ")
		if !strings.Contains(s, want) {
			t.Errorf("mode %s: want %q in %s", mode, want, s)
		}
	}
}

func TestSwapPortPlaceholder(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "linux", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 0, "${PORT}", 0), " ")
	if !strings.Contains(s, "--port ${PORT}") {
		t.Errorf("want literal ${PORT}: %s", s)
	}
}

func TestResolveContext(t *testing.T) {
	cfg := config.Defaults()
	// explicit wins
	cfg.Performance.Context = "8192"
	if got := ResolveContext(&cfg, platform.Hardware{}, 0); got != 8192 {
		t.Errorf("explicit ctx: got %d", got)
	}
	// auto with no info -> floor
	cfg.Performance.Context = "auto"
	if got := ResolveContext(&cfg, platform.Hardware{}, 0); got != ctxFloor {
		t.Errorf("auto floor: got %d", got)
	}
	// ample headroom -> large, clamped to ceiling
	got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, 4000)
	if got < ctxFloor || got > ctxCeil {
		t.Errorf("auto range: got %d", got)
	}
	// near-full VRAM -> floor
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, 15500); got != ctxFloor {
		t.Errorf("tight VRAM should floor: got %d", got)
	}
}

func TestContextLadderDescends(t *testing.T) {
	l := ContextLadder(70000)
	if len(l) == 0 || l[0] != 70000 {
		t.Fatalf("ladder head wrong: %v", l)
	}
	for i := 1; i < len(l); i++ {
		if l[i] >= l[i-1] {
			t.Fatalf("ladder not descending: %v", l)
		}
	}
	if l[len(l)-1] < 16384 {
		t.Fatalf("ladder floor too low: %v", l)
	}
}

func TestResolveMaxOutput(t *testing.T) {
	cfg := config.Defaults()
	if got := ResolveMaxOutput(&cfg, 32768); got != 16384 {
		t.Errorf("auto half: got %d", got)
	}
	if got := ResolveMaxOutput(&cfg, 200000); got != 65536 {
		t.Errorf("auto clamp: got %d", got)
	}
	cfg.Performance.MaxOutputTokens = "24000"
	if got := ResolveMaxOutput(&cfg, 65536); got != 24000 {
		t.Errorf("explicit: got %d", got)
	}
}
