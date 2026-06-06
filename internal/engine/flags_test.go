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
	s := strings.Join(ServerArgs(&cfg, hw, "model.gguf", 8080, ""), " ")
	for _, want := range []string{"-m model.gguf", "--jinja", "-ngl 99", "-c 32768", "--port 8080", "--reasoning auto", "--flash-attn on"} {
		if !strings.Contains(s, want) {
			t.Errorf("args missing %q: %s", want, s)
		}
	}
}

func TestServerArgsCPUNoOffload(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "linux", GPUVendor: "none"}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 9000, ""), " ")
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
		s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, ""), " ")
		if !strings.Contains(s, want) {
			t.Errorf("mode %s: want %q in %s", mode, want, s)
		}
	}
}

func TestSwapPortPlaceholder(t *testing.T) {
	cfg := config.Defaults()
	hw := platform.Hardware{OS: "linux", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 0, "${PORT}"), " ")
	if !strings.Contains(s, "--port ${PORT}") {
		t.Errorf("want literal ${PORT}: %s", s)
	}
}
