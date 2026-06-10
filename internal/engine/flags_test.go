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
	for _, want := range []string{"-m model.gguf", "--jinja", "-c 32768", "--port 8080", "--reasoning auto", "--flash-attn on"} {
		if !strings.Contains(s, want) {
			t.Errorf("args missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "-ngl") {
		t.Errorf("auto should omit -ngl (let llama.cpp fit layers): %s", s)
	}
}

func TestServerArgsExplicitNgl(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.GpuLayers = "40"
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 0), " ")
	if !strings.Contains(s, "-ngl 40") {
		t.Errorf("explicit gpu_layers should emit -ngl 40: %s", s)
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
	if strings.Contains(s, "-ngl") {
		t.Errorf("auto should omit -ngl (let llama.cpp fit): %s", s)
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
	if got := ResolveContext(&cfg, platform.Hardware{}, 0, false); got != 8192 {
		t.Errorf("explicit ctx: got %d", got)
	}
	// auto with no info -> floor
	cfg.Performance.Context = "auto"
	if got := ResolveContext(&cfg, platform.Hardware{}, 0, false); got != ctxFloor {
		t.Errorf("auto floor: got %d", got)
	}
	// ample headroom -> large, clamped to ceiling
	got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, 4000, false)
	if got < ctxFloor || got > ctxCeil {
		t.Errorf("auto range: got %d", got)
	}
	// near-full VRAM -> floor
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, 15500, false); got != ctxFloor {
		t.Errorf("tight VRAM should floor: got %d", got)
	}
	// experts offloaded -> aim at the ceiling regardless of the (now-irrelevant) file size
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, 15500, true); got != ctxCeil {
		t.Errorf("offloaded experts should target the ceiling: got %d", got)
	}
}

func TestResolveContextCacheType(t *testing.T) {
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 6000}
	const modelMB = 3000 // leaves a small VRAM window so factors stay below the ceiling

	cfg := config.Defaults() // q8_0 + flash_attn (defaults)
	q8 := ResolveContext(&cfg, hw, modelMB, false)

	cfg.Performance.CacheType = "f16"
	f16 := ResolveContext(&cfg, hw, modelMB, false)

	cfg.Performance.CacheType = "q4_0"
	q4 := ResolveContext(&cfg, hw, modelMB, false)

	// A smaller KV cache fits a proportionally larger window.
	if !(q4 > q8 && q8 > f16) {
		t.Errorf("expected q4 > q8 > f16, got q4=%d q8=%d f16=%d", q4, q8, f16)
	}
	// q8_0 must keep the original factor (no regression for the default).
	if q8 != 90112 {
		t.Errorf("q8_0 context changed from baseline: got %d want 90112", q8)
	}
	// Without flash attention the cache is f16 regardless of cache_type.
	cfg.Performance.FlashAttn = false // cache_type is still q4_0 here
	if got := ResolveContext(&cfg, hw, modelMB, false); got != f16 {
		t.Errorf("no flash-attn should size as f16: got %d want %d", got, f16)
	}
}

func TestResolveCPUMoEAuto(t *testing.T) {
	cfg := config.Defaults() // cpu_moe = "auto"
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16303}
	const moe = "Qwen3.6-35B-A3B-MTP-UD-IQ3_S.gguf"

	// Barely fits (free < 1024 MB) -> offload experts to free VRAM for context.
	if got := resolveCPUMoE(&cfg, hw, moe, 14636, 99); got != "all" {
		t.Errorf("barely-fitting MoE should offload: got %q", got)
	}
	// Comfortable fit -> stay fully on GPU.
	if got := resolveCPUMoE(&cfg, hw, moe, 10000, 99); got != "" {
		t.Errorf("comfortably-fitting MoE should stay on GPU: got %q", got)
	}
	// Dense model -> never offloaded by auto.
	if got := resolveCPUMoE(&cfg, hw, "Qwen3.6-27B-Q3_K_M.gguf", 14636, 99); got != "" {
		t.Errorf("dense model should not offload: got %q", got)
	}
	// Explicit off wins even when tight.
	cfg.Performance.CpuMoe = "off"
	if got := resolveCPUMoE(&cfg, hw, moe, 14636, 99); got != "" {
		t.Errorf("cpu_moe=off should never offload: got %q", got)
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

func TestIsMoEFile(t *testing.T) {
	for _, f := range []string{"Qwen3.6-35B-A3B-UD-IQ3_S.gguf", "Qwen3.6-35B-A3B-UD-Q4_K_M.gguf", "gpt-oss-20b-mxfp4.gguf"} {
		if !isMoEFile(f) {
			t.Errorf("%s should be detected as MoE", f)
		}
	}
	for _, f := range []string{"Qwen3.6-27B-Q3_K_M.gguf", "Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf", "Llama-3.2-3B-Instruct-Q4_K_M.gguf"} {
		if isMoEFile(f) {
			t.Errorf("%s should NOT be detected as MoE", f)
		}
	}
}

func TestServerArgsCpuMoe(t *testing.T) {
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 12000}

	cfg := config.Defaults()
	cfg.Performance.CpuMoe = "on"
	s := strings.Join(ServerArgs(&cfg, hw, "Qwen3.6-35B-A3B.gguf", 8080, "", 16384), " ")
	if !strings.Contains(s, "--cpu-moe") || !strings.Contains(s, "-ngl 99") {
		t.Errorf("cpu_moe=on: want -ngl 99 + --cpu-moe: %s", s)
	}

	cfg = config.Defaults()
	cfg.Performance.CpuMoe = "16"
	s = strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 16384), " ")
	if !strings.Contains(s, "--n-cpu-moe 16") {
		t.Errorf("cpu_moe=16: want --n-cpu-moe 16: %s", s)
	}

	cfg = config.Defaults()
	cfg.Performance.CpuMoe = "off"
	s = strings.Join(ServerArgs(&cfg, hw, "Qwen3.6-35B-A3B.gguf", 8080, "", 16384), " ")
	if strings.Contains(s, "cpu-moe") {
		t.Errorf("cpu_moe=off: want no cpu-moe: %s", s)
	}
}

func TestIsMTPFile(t *testing.T) {
	for _, f := range []string{"Qwen3.6-27B-MTP-Q3_K_M.gguf", "Qwen3.6-35B-A3B-MTP-UD-IQ3_S.gguf", "some-mtp-model.gguf"} {
		if !IsMTPFile(f) {
			t.Errorf("%s should be detected as MTP", f)
		}
	}
	for _, f := range []string{"Qwen3.6-27B-Q3_K_M.gguf", "Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf"} {
		if IsMTPFile(f) {
			t.Errorf("%s should NOT be detected as MTP", f)
		}
	}
}

func TestMTPArgs(t *testing.T) {
	// serverBin "" skips the support probe so this stays a pure config/filename test.
	cfg := config.Defaults() // mtp=auto, mtp_draft_max=2
	got := strings.Join(MTPArgs(&cfg, "Qwen3.6-27B-MTP-Q3_K_M.gguf", ""), " ")
	if got != "--spec-type draft-mtp --spec-draft-n-max 2" {
		t.Errorf("MTP file should yield draft-mtp flags, got %q", got)
	}
	// Non-MTP model -> no flags.
	if a := MTPArgs(&cfg, "Qwen3.6-27B-Q3_K_M.gguf", ""); a != nil {
		t.Errorf("non-MTP model should yield no MTP flags, got %v", a)
	}
	// Disabled via config.
	cfg.Performance.Mtp = "off"
	if a := MTPArgs(&cfg, "Qwen3.6-27B-MTP-Q3_K_M.gguf", ""); a != nil {
		t.Errorf("mtp=off should yield no flags, got %v", a)
	}
	// Custom draft-max.
	cfg = config.Defaults()
	cfg.Performance.MtpDraftMax = 3
	got = strings.Join(MTPArgs(&cfg, "x-MTP.gguf", ""), " ")
	if got != "--spec-type draft-mtp --spec-draft-n-max 3" {
		t.Errorf("custom mtp_draft_max not honored, got %q", got)
	}
}

func TestServerArgsExtra(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.ExtraServerArgs = []string{"--mlock", "--prio", "2"}
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 16384), " ")
	if !strings.Contains(s, "--mlock") || !strings.Contains(s, "--prio 2") {
		t.Errorf("extra_server_args not appended: %s", s)
	}
}

// Explicit --tensor-split disables the engine's own device-memory fit and can
// overpack a card (verified on real 16+12 GB hardware: cublasCreate OOM on the
// second GPU). winc must never pass it -- placement belongs to the engine.
func TestServerArgsNeverTensorSplit(t *testing.T) {
	cfg := config.Defaults()
	pair := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 28591, GPUs: []platform.GPUDevice{
		{TotalMB: 16303, FreeMB: 15054}, {TotalMB: 12288, FreeMB: 12113},
	}}
	s := strings.Join(ServerArgs(&cfg, pair, "Qwen3.6-27B-Q3_K_M.gguf", 8080, "", 16384), " ")
	if strings.Contains(s, "tensor-split") || strings.Contains(s, "split-mode") {
		t.Errorf("winc must not override the engine's multi-GPU fit: %s", s)
	}
}

// The headline win: a ~22 GB MoE on a 16+12 GB pair runs fully on GPU with a big
// context, where the 16 GB card alone is forced to offload its experts to RAM.
func TestMultiGPUMoEFitsWithoutOffload(t *testing.T) {
	cfg := config.Defaults()
	moe := "Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf"
	single := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16303, GPUs: []platform.GPUDevice{{TotalMB: 16303}}}
	pair := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 28591, GPUs: []platform.GPUDevice{
		{TotalMB: 16303, FreeMB: 15054}, {TotalMB: 12288, FreeMB: 12113},
	}}
	if _, got := PlanForModel(&cfg, single, moe, 21600); got != "all" {
		t.Errorf("21.6 GB MoE on one 16 GB card should offload experts, got %q", got)
	}
	ctx, got := PlanForModel(&cfg, pair, moe, 21600)
	if got != "" {
		t.Errorf("21.6 GB MoE on a 28 GB pair should stay fully on GPU, got %q", got)
	}
	if ctx < 65536 {
		t.Errorf("pair should afford a large context, got %d", ctx)
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
