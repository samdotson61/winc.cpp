package engine

import (
	"os"
	"path/filepath"
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
	if got := ResolveContext(&cfg, platform.Hardware{}, "m.gguf", 0, false); got != 8192 {
		t.Errorf("explicit ctx: got %d", got)
	}
	// auto with no info -> floor
	cfg.Performance.Context = "auto"
	if got := ResolveContext(&cfg, platform.Hardware{}, "m.gguf", 0, false); got != ctxFloor {
		t.Errorf("auto floor: got %d", got)
	}
	// ample headroom -> large, clamped to ceiling
	got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m.gguf", 4000, false)
	if got < ctxFloor || got > ctxCeil {
		t.Errorf("auto range: got %d", got)
	}
	// near-full VRAM -> the bottom target (engine fit spills layers), not a
	// floor smaller than the agent's own fixed overhead.
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m.gguf", 15500, false); got != BottomCtxTokens {
		t.Errorf("tight VRAM should target the bottom window via partial offload: got %d", got)
	}
	// Offloaded experts aim at the full ceiling (most VRAM is free for KV).
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m.gguf", 15500, true); got != ctxCeil {
		t.Errorf("offloaded experts should target the ceiling: got %d", got)
	}
	// One universal policy: "optimal" and "auto" both aim at the ceiling; the
	// ladder + fit oracle + placement gate settle the largest TRUE window.
	roomy := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 28591}
	cfg.Performance.Context = "optimal"
	optimal := ResolveContext(&cfg, roomy, "m.gguf", 5000, false)
	cfg.Performance.Context = "auto"
	auto := ResolveContext(&cfg, roomy, "m.gguf", 5000, false)
	if optimal != auto {
		t.Errorf("optimal and auto are the same universal policy: %d vs %d", optimal, auto)
	}
	if optimal != ctxCeil {
		t.Errorf("roomy hardware should target the ceiling, got %d", optimal)
	}
	// An MTP model's draft context eats VRAM the KV cache can't use: same sizes,
	// smaller window.
	plain := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m.gguf", 12000, false)
	mtp := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m-MTP.gguf", 12000, false)
	if mtp >= plain {
		t.Errorf("MTP draft context should shrink the window: mtp=%d plain=%d", mtp, plain)
	}
	// mtp = "off" -> no draft context -> no allowance.
	cfg.Performance.Mtp = "off"
	if got := ResolveContext(&cfg, platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000}, "m-MTP.gguf", 12000, false); got != plain {
		t.Errorf("mtp=off should size like a plain model: got %d want %d", got, plain)
	}
}

func TestResolveContextCacheType(t *testing.T) {
	// Sized so every cache type lands BETWEEN the usable floor and the auto
	// ceiling -- the factor ordering is only observable in that band.
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000, GPUs: []platform.GPUDevice{{TotalMB: 16000}}}
	const modelMB = 12500

	cfg := config.Defaults() // q8_0 + flash_attn (defaults)
	cfg.Performance.Context = "auto"
	q8 := ResolveContext(&cfg, hw, "m.gguf", modelMB, false)

	cfg.Performance.CacheType = "f16"
	f16 := ResolveContext(&cfg, hw, "m.gguf", modelMB, false)

	cfg.Performance.CacheType = "q4_0"
	q4 := ResolveContext(&cfg, hw, "m.gguf", modelMB, false)

	// A smaller KV cache fits a proportionally larger window.
	if !(q4 > q8 && q8 > f16) {
		t.Errorf("expected q4 > q8 > f16, got q4=%d q8=%d f16=%d", q4, q8, f16)
	}
	// q8_0 must keep the original 64 tokens/MB factor (no regression for the
	// default): free = 16000 - 12500 - 1536 = 1964 MB -> 1964*64 rounded down
	// to the 8192 grid.
	if q8 != 122880 {
		t.Errorf("q8_0 context changed from baseline: got %d want 122880", q8)
	}
	// Without flash attention the cache is f16 regardless of cache_type.
	cfg.Performance.FlashAttn = false // cache_type is still q4_0 here
	if got := ResolveContext(&cfg, hw, "m.gguf", modelMB, false); got != f16 {
		t.Errorf("no flash-attn should size as f16: got %d want %d", got, f16)
	}
}

// A head model that fully fits combined VRAM gets every layer forced onto the GPU
// (-ngl 99), so the engine's conservative device fit can't spill one to the CPU --
// the CPU belongs to the team's workers. A model that doesn't fit keeps -ngl on
// auto for the engine's partial offload.
func TestServerArgsForcesFullGPUWhenFits(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "Dense-27B-Q4_K_M.gguf")
	f, err := os.Create(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100 << 20); err != nil { // 100 MB stand-in
		t.Fatal(err)
	}
	f.Close()

	cfg := config.Defaults()
	fits := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, fits, model, 8080, "", 16384), " ")
	if !strings.Contains(s, "-ngl 99") {
		t.Errorf("fully-fitting model should force -ngl 99: %s", s)
	}
	// Too little VRAM for model+reserve+KV -> leave -ngl to the engine's fit.
	tight := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 1600}
	s = strings.Join(ServerArgs(&cfg, tight, model, 8080, "", 16384), " ")
	if strings.Contains(s, "-ngl") {
		t.Errorf("partial-fit model should leave -ngl on auto: %s", s)
	}
	// Explicit gpu_layers still wins over the full-fit rule.
	cfg.Performance.GpuLayers = "20"
	s = strings.Join(ServerArgs(&cfg, fits, model, 8080, "", 16384), " ")
	if !strings.Contains(s, "-ngl 20") {
		t.Errorf("explicit gpu_layers should win: %s", s)
	}
	// Apple unified memory keeps its existing behavior (no forced -ngl).
	cfg.Performance.GpuLayers = "auto"
	mac := platform.Hardware{OS: "darwin", GPUVendor: "apple", Unified: true, RAMMB: 32768, VRAMMB: 32768}
	s = strings.Join(ServerArgs(&cfg, mac, model, 8080, "", 16384), " ")
	if strings.Contains(s, "-ngl") {
		t.Errorf("unified memory should not force -ngl: %s", s)
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

// cache_type = "auto" downshifts to the ASYMMETRIC q8_0/q4_0 pair (keys keep
// precision, values compress) when the q8-sized window would be starved; ample
// setups keep q8_0, and explicit values are honored.
func TestAutoKVCacheDownshift(t *testing.T) {
	cfg := config.Defaults() // cache_type = "auto"
	tight := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 8192}
	if got := EffectiveCacheType(&cfg, tight, "m.gguf", 6200, false); got != "q8_0/q4_0" {
		t.Errorf("starved window should downshift to q8_0/q4_0, got %q", got)
	}
	// The raw asym window (705 MB free -> ~57k) is still under the bottom
	// target, so sizing aims at the bottom and placement spills the difference.
	if got := ResolveContext(&cfg, tight, "m.gguf", 6200, false); got != BottomCtxTokens {
		t.Errorf("downshifted window = %d, want %d", got, BottomCtxTokens)
	}
	ample := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 28591}
	if got := EffectiveCacheType(&cfg, ample, "m.gguf", 13800, false); got != "q8_0" {
		t.Errorf("ample window should stay q8_0, got %q", got)
	}
	// Explicit q8_0 never downshifts the CACHE; the window that pure-q8 KV can't
	// reach lands at the usable floor instead (partial offload places the rest).
	cfg.Performance.CacheType = "q8_0"
	if got := ResolveContext(&cfg, tight, "m.gguf", 6200, false); got != BottomCtxTokens {
		t.Errorf("explicit q8_0 sizes to the bottom target: got %d", got)
	}
	// Without flash attention the cache is f16 regardless -- auto stays q8_0.
	cfg.Performance.CacheType = "auto"
	cfg.Performance.FlashAttn = false
	if got := EffectiveCacheType(&cfg, tight, "m.gguf", 6200, false); got != "q8_0" {
		t.Errorf("no flash-attn: auto must stay q8_0, got %q", got)
	}
	// Unknown model size never downshifts.
	cfg.Performance.FlashAttn = true
	if got := EffectiveCacheType(&cfg, tight, "missing.gguf", 0, false); got != "q8_0" {
		t.Errorf("unknown size must stay q8_0, got %q", got)
	}
}

// Asymmetric pairs split correctly and size harmonically (bytes add per side).
func TestSplitKVAndPairFactor(t *testing.T) {
	if k, v := SplitKV("q8_0/q4_0"); k != "q8_0" || v != "q4_0" {
		t.Errorf("SplitKV pair wrong: %q %q", k, v)
	}
	if k, v := SplitKV("q8_0"); k != "q8_0" || v != "q8_0" {
		t.Errorf("SplitKV plain wrong: %q %q", k, v)
	}
	if got := kvCtxFactor("q8_0/q4_0", true); got != 83 { // 2*64*120/(64+120)
		t.Errorf("pair factor = %d, want 83", got)
	}
	if got := kvCtxFactor("q8_0", true); got != 64 {
		t.Errorf("plain q8 factor = %d, want 64", got)
	}
	if got := kvCtxFactor("q8_0/q4_0", false); got != 32 {
		t.Errorf("no flash-attn must size as f16, got %d", got)
	}
}

// ServerArgs and MTPArgs emit the split flags for an asymmetric cache.
func TestAsymmetricCacheFlags(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.CacheType = "q8_0/q4_0"
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}
	s := strings.Join(ServerArgs(&cfg, hw, "m.gguf", 8080, "", 16384), " ")
	if !strings.Contains(s, "--cache-type-k q8_0 --cache-type-v q4_0") {
		t.Errorf("asymmetric main cache flags missing: %s", s)
	}
	m := strings.Join(MTPArgs(&cfg, hw, "x-MTP.gguf", ""), " ")
	if !strings.Contains(m, "--spec-draft-type-k q8_0 --spec-draft-type-v q4_0") {
		t.Errorf("asymmetric draft cache flags missing: %s", m)
	}
}

// The ceiling follows the 2026 catalog (every model is natively >=256K).
func TestContextLadderCeiling(t *testing.T) {
	l := ContextLadder(262144)
	if l[0] != 262144 || l[1] != 196608 || l[2] != 131072 {
		t.Errorf("ceiling ladder rungs wrong: %v", l)
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
	cfg := config.Defaults() // mtp=auto, mtp_draft_max=2, cache auto (unknown size -> q8_0)
	hw := platform.Hardware{}
	got := strings.Join(MTPArgs(&cfg, hw, "Qwen3.6-27B-MTP-Q3_K_M.gguf", ""), " ")
	want := "--spec-type draft-mtp --spec-draft-n-max 2 --spec-draft-type-k q8_0 --spec-draft-type-v q8_0"
	if got != want {
		t.Errorf("MTP file should yield draft-mtp flags + a quantized draft cache:\n got %q\nwant %q", got, want)
	}
	// Non-MTP model -> no flags.
	if a := MTPArgs(&cfg, hw, "Qwen3.6-27B-Q3_K_M.gguf", ""); a != nil {
		t.Errorf("non-MTP model should yield no MTP flags, got %v", a)
	}
	// Disabled via config.
	cfg.Performance.Mtp = "off"
	if a := MTPArgs(&cfg, hw, "Qwen3.6-27B-MTP-Q3_K_M.gguf", ""); a != nil {
		t.Errorf("mtp=off should yield no flags, got %v", a)
	}
	// Custom draft-max.
	cfg = config.Defaults()
	cfg.Performance.MtpDraftMax = 3
	got = strings.Join(MTPArgs(&cfg, hw, "x-MTP.gguf", ""), " ")
	if !strings.Contains(got, "--spec-draft-n-max 3") {
		t.Errorf("custom mtp_draft_max not honored, got %q", got)
	}
	// Without flash attention the draft cache stays at the engine default.
	cfg = config.Defaults()
	cfg.Performance.FlashAttn = false
	if got := strings.Join(MTPArgs(&cfg, hw, "x-MTP.gguf", ""), " "); strings.Contains(got, "spec-draft-type") {
		t.Errorf("no flash-attn: draft cache must not be quantized, got %q", got)
	}
}

// Gemma 4 ships MTP heads as a separate small GGUF; winc pairs a downloaded head
// with every quant of its model family by filename prefix and passes it as the
// draft model. Qwen-style baked-in MTP keeps the spec type only.
func TestMTPHeadPairing(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gemma := touch("gemma-4-26B-A4B-it-UD-IQ4_NL.gguf")
	head := touch("gemma-4-26B-A4B-it-Q8_0-MTP.gguf")
	touch("gemma-4-E2B-it-Q8_0-MTP.gguf") // another family's head must not pair

	cfg := config.Defaults()
	hw := platform.Hardware{}
	got := strings.Join(MTPArgs(&cfg, hw, gemma, ""), " ")
	want := "--spec-type draft-mtp --spec-draft-n-max 2 --spec-draft-model " + head +
		" --spec-draft-type-k q8_0 --spec-draft-type-v q8_0"
	if got != want {
		t.Errorf("external head pairing wrong:\n got %q\nwant %q", got, want)
	}
	// A model whose family has no downloaded head stays plain.
	plain := touch("gemma-4-31B-it-Q3_K_M.gguf")
	if a := MTPArgs(&cfg, hw, plain, ""); a != nil {
		t.Errorf("no matching head should mean no MTP flags, got %v", a)
	}
	// Baked-in MTP (Qwen) keeps the spec type only -- no draft model flag.
	qwen := touch("Qwen3.6-27B-MTP-Q3_K_M.gguf")
	if got := strings.Join(MTPArgs(&cfg, hw, qwen, ""), " "); strings.Contains(got, "spec-draft-model") || !strings.Contains(got, "draft-mtp") {
		t.Errorf("baked-in MTP should not get a draft model: %q", got)
	}
	// mtp=off disables head pairing too.
	cfg.Performance.Mtp = "off"
	if a := MTPArgs(&cfg, hw, gemma, ""); a != nil {
		t.Errorf("mtp=off should disable head pairing, got %v", a)
	}
}

// The fit calculator's output line parses into the fitted -ngl.
func TestParseFitNGL(t *testing.T) {
	cases := map[string]int{
		"-c 131072 -ngl 65 -ts 39,26":                         65,
		"-c 16384 -ngl -1":                                    -1,
		`-c 131072 -ngl 41 -ts 26,15 -ot "blk\.25\..*=CUDA1"`: 41,
	}
	for in, want := range cases {
		got, ok := parseFitNGL(in)
		if !ok || got != want {
			t.Errorf("parseFitNGL(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := parseFitNGL("no flags here"); ok {
		t.Error("garbage should not parse")
	}
}

// NextLadderRung climbs the standard rungs and stops at the ceiling.
func TestNextLadderRung(t *testing.T) {
	cases := map[int]int{10000: 49152, 98304: 131072, 131072: 196608, 196608: 262144, 262144: 262144}
	for in, want := range cases {
		if got := NextLadderRung(in); got != want {
			t.Errorf("NextLadderRung(%d) = %d, want %d", in, got, want)
		}
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

// ForcedFullGPU marks exactly the loads the launcher's placement gate verifies:
// auto gpu_layers + a comfortable full-VRAM fit. Explicit settings, unified
// memory, oversized models, and expert-offloaded MoEs are never gated. Sizes
// are supplied directly (forcedFullGPUAt): fixture files at real model sizes
// stay sparse on POSIX but are fully allocated on NTFS, which exhausted the
// Windows CI runner's disk.
func TestForcedFullGPU(t *testing.T) {
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000}

	cfg := config.Defaults()
	if !forcedFullGPUAt(&cfg, hw, "Dense-9B-Q5_K_M.gguf", 8000) {
		t.Error("a comfortably fitting dense model on auto must be force-pinned (and gated)")
	}
	if forcedFullGPUAt(&cfg, hw, "Dense-30B-Q5_K_M.gguf", 15500) {
		t.Error("a model that leaves no KV headroom is not a forced full fit")
	}
	if forcedFullGPUAt(&cfg, hw, "Moe-30B-A3B-Q4_K_M.gguf", 15500) {
		t.Error("a MoE that auto-offloads its experts is not gated")
	}
	if !forcedFullGPUAt(&cfg, hw, "Moe-9B-A3B-Q4_K_M.gguf", 8000) {
		t.Error("a comfortably fitting MoE stays fully on GPU and is gated")
	}
	uni := platform.Hardware{OS: "darwin", GPUVendor: "apple", VRAMMB: 24000, Unified: true}
	if forcedFullGPUAt(&cfg, uni, "Dense-9B-Q5_K_M.gguf", 8000) {
		t.Error("unified memory keeps the engine's own placement and is never gated")
	}
	cfg.Performance.GpuLayers = "99"
	if forcedFullGPUAt(&cfg, hw, "Dense-9B-Q5_K_M.gguf", 8000) {
		t.Error("explicit gpu_layers runs as written and is never gated")
	}
}

// Small models reserve proportionally less VRAM for compute buffers; >= 8 GB
// models keep the calibrated 1536 exactly. The flat reserve ate a 4 GB card
// whole (4096 - 2800 model - 1536 reserve < 0 -> sizing collapsed to the floor).
func TestGpuReserveScalesWithModel(t *testing.T) {
	one := platform.Hardware{GPUs: []platform.GPUDevice{{TotalMB: 4096}}}
	if got, want := gpuReserveMB(one, 2800), 512+2800/8; got != want {
		t.Errorf("4B-class reserve = %d, want %d", got, want)
	}
	if got := gpuReserveMB(one, 18915); got != 1536 {
		t.Errorf("big-model reserve must stay the calibrated 1536, got %d", got)
	}
	if got := gpuReserveMB(one, 0); got != 1536 {
		t.Errorf("unknown model size stays conservative, got %d", got)
	}
	two := platform.Hardware{GPUs: []platform.GPUDevice{{TotalMB: 16000}, {TotalMB: 12000}}}
	if got := gpuReserveMB(two, 18915); got != 1536+768 {
		t.Errorf("multi-GPU adds 768 per extra card, got %d", got)
	}
}

// A 4 GB card with the 4B: full-GPU sizing cannot reach a workable agent window,
// so the target becomes the usable floor and the engine's device fit places the
// layers (partial offload) -- instead of collapsing to a window smaller than
// Claude Code's own fixed overhead.
func TestResolveContextTinyCard(t *testing.T) {
	cfg := config.Defaults()
	tiny := platform.Hardware{OS: "linux", GPUVendor: "nvidia", VRAMMB: 4096, GPUs: []platform.GPUDevice{{TotalMB: 4096}}}
	if got := ResolveContext(&cfg, tiny, "Qwen3.5-4B-Q4_K_M.gguf", 2800, false); got != BottomCtxTokens {
		t.Errorf("4 GB + 4B should target the bottom window via partial offload, got %d", got)
	}
	// And the ladder must be allowed to TRY it: a partial fit is never vetoed by
	// the full-GPU oracle (ForcedFullGPU is false on this hardware).
	if forcedFullGPUAt(&cfg, tiny, "Qwen3.5-4B-Q4_K_M.gguf", 2800) {
		t.Error("a 4 GB card must not be treated as a forced-full-GPU fit")
	}
	// A roomy fit keeps full-GPU sizing exactly as before.
	roomy := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 16000, GPUs: []platform.GPUDevice{{TotalMB: 16000}}}
	if got := ResolveContext(&cfg, roomy, "Dense-9B-Q5_K_M.gguf", 8000, false); got < BottomCtxTokens {
		t.Errorf("a roomy fit must not shrink, got %d", got)
	}
}
