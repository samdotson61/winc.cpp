package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"winc/internal/config"
	"winc/internal/platform"
)

// moeNamePat matches the MoE naming convention: an active-param tag like "A3B" /
// "A22B" / "A1.4B", or "moe"/"gpt-oss" in the filename.
var moeNamePat = regexp.MustCompile(`(?i)(^|[-_.])a\d+(\.\d+)?b([-_.]|$)|moe|gpt-oss`)

// isMoEFile guesses whether a GGUF is a Mixture-of-Experts model from its name.
func isMoEFile(path string) bool { return moeNamePat.MatchString(filepath.Base(path)) }

// IsMoEFile reports whether a GGUF filename looks like a Mixture-of-Experts model.
func IsMoEFile(path string) bool { return isMoEFile(path) }

// resolveCPUMoE decides MoE expert offload: "" (none), "all" (--cpu-moe), or an
// integer layer count (--n-cpu-moe N). Auto offloads only when a MoE model won't
// fit VRAM, so models that fit stay fully on the GPU (fastest generation). modelMB
// is the model's on-disk size (0 = unknown; auto can't size-check, so it won't
// engage offload).
func resolveCPUMoE(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB, ngl int) string {
	switch v := strings.ToLower(strings.TrimSpace(cfg.Performance.CpuMoe)); v {
	case "off", "false", "no":
		return ""
	case "on", "all", "true", "yes":
		return "all"
	case "", "auto":
		// fall through to auto logic
	default:
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return strconv.Itoa(n)
		}
	}
	if ngl == 0 || hw.VRAMMB <= 0 || !isMoEFile(modelPath) {
		return ""
	}
	if modelMB > 0 && modelMB+1536 > hw.VRAMMB {
		return "all"
	}
	return ""
}

// isMTPFile reports whether a GGUF is a Multi-Token-Prediction variant (winc saves
// those with "-MTP-" in the local name; upstream MTP repos use "mtp" too).
func isMTPFile(path string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(path)), "mtp")
}

// IsMTPFile reports whether a GGUF filename looks like an MTP (Multi-Token Prediction) variant.
func IsMTPFile(path string) bool { return isMTPFile(path) }

var (
	mtpProbeMu sync.Mutex
	mtpProbe   = map[string]bool{}
)

// serverSupportsMTP reports whether a llama-server binary understands the MTP flag.
// Result is cached per binary path (--help is cheap but we may ask several times).
func serverSupportsMTP(bin string) bool {
	mtpProbeMu.Lock()
	defer mtpProbeMu.Unlock()
	if v, ok := mtpProbe[bin]; ok {
		return v
	}
	cmd := exec.Command(bin, "--help")
	cmd.Env = mtpProbeEnv(bin)
	out, _ := cmd.CombinedOutput()
	ok := strings.Contains(string(out), "draft-mtp")
	mtpProbe[bin] = ok
	return ok
}

// mtpProbeEnv makes co-located shared libraries loadable for the --help probe.
func mtpProbeEnv(bin string) []string {
	dir := filepath.Dir(bin)
	env := os.Environ()
	switch runtime.GOOS {
	case "linux":
		env = append(env, "LD_LIBRARY_PATH="+dir+string(os.PathListSeparator)+os.Getenv("LD_LIBRARY_PATH"))
	case "darwin":
		env = append(env, "DYLD_LIBRARY_PATH="+dir+string(os.PathListSeparator)+os.Getenv("DYLD_LIBRARY_PATH"))
	}
	return env
}

// MTPArgs returns the Multi-Token-Prediction flags for an MTP model, or nil when the
// model isn't MTP, config disables it, or the engine is too old to support it (pass
// serverBin to probe; "" skips the probe). Never breaks a launch -- a model that fits
// MTP but lacks engine support simply runs without it.
func MTPArgs(cfg *config.Config, modelPath, serverBin string) []string {
	if strings.EqualFold(strings.TrimSpace(cfg.Performance.Mtp), "off") {
		return nil
	}
	if !isMTPFile(modelPath) {
		return nil
	}
	if serverBin != "" && !serverSupportsMTP(serverBin) {
		return nil
	}
	n := cfg.Performance.MtpDraftMax
	if n <= 0 {
		n = 2
	}
	return []string{"--spec-type", "draft-mtp", "--spec-draft-n-max", strconv.Itoa(n)}
}

// PlanForModel reports the context window and MoE-offload decision winc would use
// for a model file of the given on-disk size in MB (0 = unknown). For diagnostics
// (winc detect). cpuMoe is "" (none / full GPU), "all" (--cpu-moe), or a layer count.
func PlanForModel(cfg *config.Config, hw platform.Hardware, modelFile string, modelMB int) (ctx int, cpuMoe string) {
	ctx = ResolveContext(cfg, hw, modelMB)
	cpuMoe = resolveCPUMoE(cfg, hw, modelFile, modelMB, GpuLayers(cfg, hw))
	return ctx, cpuMoe
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// FileMB returns a file's size in MB (0 if unknown).
func FileMB(path string) int {
	if fi, err := os.Stat(path); err == nil {
		return int(fi.Size() / (1024 * 1024))
	}
	return 0
}

// GpuLayers resolves the -ngl value from config + hardware.
func GpuLayers(cfg *config.Config, hw platform.Hardware) int {
	if cfg.Performance.GpuLayers == "auto" || cfg.Performance.GpuLayers == "" {
		if hw.GPUVendor != "" && hw.GPUVendor != "none" {
			return 99
		}
		return 0
	}
	return atoiOr(cfg.Performance.GpuLayers, 99)
}

const (
	ctxFloor = 32768 // enough for Claude Code's system prompt + tools + headroom
	ctxCeil  = 131072
)

// kvCtxFactor is the auto-context multiplier (tokens per free MB of VRAM) for a KV
// cache type. q8_0 (~16 KB/token) is the baseline 64; f16 doubles the bytes (so
// halves the tokens), q4 halves the bytes (so doubles the tokens). Conservative.
// KV quantization needs flash attention -- without it the cache is f16 regardless.
func kvCtxFactor(cacheType string, flashAttn bool) int {
	if !flashAttn {
		return 32 // f16 K+V
	}
	switch strings.ToLower(strings.TrimSpace(cacheType)) {
	case "f16", "":
		return 32
	case "q8_0":
		return 64
	case "q5_0", "q5_1":
		return 80
	case "q4_0", "q4_1":
		return 120
	default:
		return 64 // unknown -> conservative q8 baseline
	}
}

// ResolveContext picks a liberal context window: the configured value, or (auto)
// the largest that should fit free VRAM after the model, clamped to a safe range.
// The launcher verifies the choice actually loads and falls back if not.
func ResolveContext(cfg *config.Config, hw platform.Hardware, modelFileMB int) int {
	if cfg.Performance.Context != "auto" && cfg.Performance.Context != "" {
		return atoiOr(cfg.Performance.Context, ctxFloor)
	}
	if GpuLayers(cfg, hw) == 0 || hw.VRAMMB <= 0 || modelFileMB <= 0 {
		return ctxFloor
	}
	free := hw.VRAMMB - modelFileMB - 1536 // reserve compute buffer + safety
	if free <= 0 {
		return ctxFloor
	}
	// Bytes/token depends on the KV cache type, so a smaller cache (q4) fits a
	// proportionally larger window. Default q8_0 keeps the original factor (64).
	toks := free * kvCtxFactor(cfg.Performance.CacheType, cfg.Performance.FlashAttn)
	toks = (toks / 8192) * 8192
	if toks < ctxFloor {
		return ctxFloor
	}
	if toks > ctxCeil {
		return ctxCeil
	}
	return toks
}

// ContextLadder returns descending context sizes to try (largest fitting first),
// always bottoming out at a workable floor.
func ContextLadder(target int) []int {
	steps := []int{target, 98304, 65536, 49152, 32768, 24576, 16384}
	var out []int
	seen := map[int]bool{}
	for _, s := range steps {
		if s <= target && s >= 16384 && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	if len(out) == 0 {
		out = []int{16384}
	}
	return out
}

// ResolveMaxOutput caps the agent's response length: configured value, or (auto)
// ~half the context, clamped so the prompt always has room.
func ResolveMaxOutput(cfg *config.Config, loadedCtx int) int {
	if cfg.Performance.MaxOutputTokens != "auto" && cfg.Performance.MaxOutputTokens != "" {
		return atoiOr(cfg.Performance.MaxOutputTokens, loadedCtx/2)
	}
	v := loadedCtx / 2
	if v > 65536 {
		v = 65536
	}
	if v > loadedCtx-2048 {
		v = loadedCtx - 2048
	}
	if v < 4096 {
		v = 4096
	}
	return v
}

// ServerArgs assembles llama-server arguments. ctx<=0 resolves automatically.
// portPlaceholder, if set, replaces the numeric port (llama-swap needs "${PORT}").
func ServerArgs(cfg *config.Config, hw platform.Hardware, modelPath string, port int, portPlaceholder string, ctx int) []string {
	portVal := strconv.Itoa(port)
	if portPlaceholder != "" {
		portVal = portPlaceholder
	}
	args := []string{"-m", modelPath, "--host", cfg.General.Host, "--port", portVal, "--jinja"}

	ngl := GpuLayers(cfg, hw)
	// MoE expert offload: keep all layers on the GPU (-ngl 99) but move MoE expert
	// weights to RAM, so a MoE bigger than VRAM still runs fast (only a small
	// activation vector crosses PCIe). Otherwise: force -ngl only when explicitly
	// set; for "auto" omit it so llama.cpp fits layers to memory (partial offload).
	switch cpuMoe := resolveCPUMoE(cfg, hw, modelPath, FileMB(modelPath), ngl); {
	case cpuMoe == "all":
		args = append(args, "-ngl", "99", "--cpu-moe")
	case cpuMoe != "":
		args = append(args, "-ngl", "99", "--n-cpu-moe", cpuMoe)
	case cfg.Performance.GpuLayers != "auto" && cfg.Performance.GpuLayers != "":
		args = append(args, "-ngl", strconv.Itoa(ngl))
	}

	if ctx <= 0 {
		ctx = ResolveContext(cfg, hw, FileMB(modelPath))
	}
	args = append(args, "-c", strconv.Itoa(ctx))

	// Batch sizes: auto tunes prompt-processing throughput when offloading.
	if cfg.Performance.Batch == "auto" || cfg.Performance.Batch == "" {
		if ngl > 0 {
			args = append(args, "-b", "2048", "-ub", "512")
		}
	} else {
		args = append(args, "-b", cfg.Performance.Batch)
	}

	// Flash attention + quantized KV cache only when offloading to GPU.
	if cfg.Performance.FlashAttn && ngl > 0 {
		args = append(args, "--flash-attn", "on")
		if ct := cfg.Performance.CacheType; ct != "" && ct != "f16" {
			args = append(args, "--cache-type-k", ct, "--cache-type-v", ct)
		}
	}

	if cfg.Performance.Threads != "auto" && cfg.Performance.Threads != "" {
		args = append(args, "-t", cfg.Performance.Threads)
	}

	// Reasoning: static modes set server flags; adaptive runs in "auto" and lets
	// winc-router cap the budget per request.
	switch cfg.Reasoning.Mode {
	case "off":
		args = append(args, "--reasoning-budget", "0")
	case "on":
		args = append(args, "--reasoning", "on")
	case "fixed":
		args = append(args, "--reasoning-budget", strconv.Itoa(cfg.Reasoning.FixedBudgetTokens))
	default: // adaptive
		args = append(args, "--reasoning", "auto")
	}

	// Speculative decoding: a small same-family draft model (in the same dir as the
	// main model) predicts tokens the main model verifies in a batch.
	if d := strings.TrimSpace(cfg.Performance.DraftModel); d != "" {
		dp := d
		if !filepath.IsAbs(dp) {
			dp = filepath.Join(filepath.Dir(modelPath), d)
		}
		if fi, err := os.Stat(dp); err == nil && !fi.IsDir() {
			args = append(args, "--spec-draft-model", dp)
		}
	}

	// Advanced escape hatch: any extra llama-server flags, verbatim.
	args = append(args, cfg.Performance.ExtraServerArgs...)
	return args
}
