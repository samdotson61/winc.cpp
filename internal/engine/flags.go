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

// minKVHeadroomMB is the free VRAM (after model + compute buffer) below which a MoE
// model's context would be stuck near the floor. Auto-offload kicks in under this so
// the experts move to RAM and free VRAM for a usable context.
const minKVHeadroomMB = 1024

// gpuReserveMB is the VRAM held back from sizing math for compute buffers, driver
// overhead, and desktop use -- per device, since each GPU in a multi-GPU split
// allocates its own compute buffer.
func gpuReserveMB(hw platform.Hardware) int {
	n := len(hw.GPUs)
	if n < 1 {
		n = 1
	}
	return 1536 + 768*(n-1)
}

// resolveCPUMoE decides MoE expert offload: "" (none), "all" (--cpu-moe), or an
// integer layer count (--n-cpu-moe N). Auto offloads a MoE model when it won't fit
// VRAM OR when it fits so tightly that almost no VRAM is left for KV (context would
// be stuck at the floor) -- moving experts to RAM then frees VRAM for a much larger
// context, trading some expert-compute speed (MTP / the small active set softens it).
// Comfortably-fitting models stay fully on the GPU (fastest). modelMB is the on-disk
// size (0 = unknown; auto can't size-check, so it won't engage offload).
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
	if ngl == 0 || hw.VRAMMB <= 0 || modelMB <= 0 || !isMoEFile(modelPath) {
		return ""
	}
	if hw.VRAMMB-modelMB-gpuReserveMB(hw) < minKVHeadroomMB {
		return "all"
	}
	return ""
}

// WillOffloadExperts reports whether winc will move this model's MoE experts to RAM
// (--cpu-moe) -- which frees most of the model's VRAM for a much larger KV cache.
func WillOffloadExperts(cfg *config.Config, hw platform.Hardware, modelPath string) bool {
	return resolveCPUMoE(cfg, hw, modelPath, FileMB(modelPath), GpuLayers(cfg, hw)) == "all"
}

// mainEscalationHeadroomMB is the free VRAM (after the main model + compute buffer)
// required before winc will let subagents escalate onto the main GPU model. Below this,
// escalation tops out at the CPU worker so the orchestrator stays responsive and the KV
// cache isn't starved by extra concurrent sequences.
const mainEscalationHeadroomMB = 6000

// MainEscalationOK reports whether the main GPU model has enough spare VRAM to also
// serve escalated subagents concurrently. False when there's no GPU, when experts are
// offloaded to RAM (the main model is already compute-compromised), or when free VRAM
// after the model is below the headroom threshold -- in those cases escalation stops at
// the CPU worker.
func MainEscalationOK(cfg *config.Config, hw platform.Hardware, modelPath string) bool {
	if GpuLayers(cfg, hw) <= 0 || hw.VRAMMB <= 0 {
		return false
	}
	if WillOffloadExperts(cfg, hw, modelPath) {
		return false
	}
	mb := FileMB(modelPath)
	if mb <= 0 {
		return false
	}
	return hw.VRAMMB-mb-gpuReserveMB(hw) >= mainEscalationHeadroomMB
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

var (
	helpMu    sync.Mutex
	helpCache = map[string]string{}
)

// serverHelp returns the (cached) `--help` output of a llama-server binary, used to feature-
// detect optional flags so an older engine can't break a launch.
func serverHelp(bin string) string {
	if bin == "" {
		return ""
	}
	helpMu.Lock()
	defer helpMu.Unlock()
	if v, ok := helpCache[bin]; ok {
		return v
	}
	cmd := exec.Command(bin, "--help")
	cmd.Env = mtpProbeEnv(bin)
	out, _ := cmd.CombinedOutput()
	helpCache[bin] = string(out)
	return helpCache[bin]
}

// serverSupportsFlag reports whether the engine's --help mentions a flag.
func serverSupportsFlag(bin, flag string) bool {
	return bin != "" && strings.Contains(serverHelp(bin), flag)
}

// CacheReuseArgs enables KV-shift prompt-cache reuse (recovers the prefix cache after a
// small mid-prompt change) when the engine supports it. Prompt-prefix caching itself is on
// by default; this just extends it. Probed via --help, so an older build that lacks the
// flag simply runs without it.
func CacheReuseArgs(serverBin string) []string {
	if serverSupportsFlag(serverBin, "--cache-reuse") {
		return []string{"--cache-reuse", "256"}
	}
	return nil
}

// mtpHeadFor finds an external MTP drafter head next to a model: a small
// "<family>-<quant>-MTP.gguf" file (Gemma 4 ships its prediction heads as a
// separate GGUF; Qwen bakes them into the model). A head pairs when the model's
// filename starts with the head's family prefix, so one Q8_0 head serves every
// quant of its model. Returns "" when no head matches.
func mtpHeadFor(modelPath string) string {
	base := filepath.Base(modelPath)
	dir := filepath.Dir(modelPath)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || n == base || !strings.HasSuffix(strings.ToLower(n), "-mtp.gguf") {
			continue
		}
		fam := n[:len(n)-len("-mtp.gguf")] // gemma-4-26B-A4B-it-Q8_0
		if i := strings.LastIndex(fam, "-"); i > 0 {
			fam = fam[:i] // strip the head's own quant tag -> gemma-4-26B-A4B-it
		}
		if fam != "" && strings.HasPrefix(base, fam+"-") {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// MTPArgs returns the Multi-Token-Prediction flags, or nil when the model has no
// MTP (neither baked-in heads nor a downloaded external head), config disables it,
// or the engine is too old to support it (pass serverBin to probe; "" skips the
// probe). A baked-in MTP model (filename contains "MTP") needs only the spec type;
// an external head (Gemma 4) is additionally passed as the draft model. Never
// breaks a launch -- a model that fits MTP but lacks engine support simply runs
// without it.
func MTPArgs(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string) []string {
	if !mtpActive(cfg, modelPath) {
		return nil
	}
	if serverBin != "" && !serverSupportsMTP(serverBin) {
		return nil
	}
	n := cfg.Performance.MtpDraftMax
	if n <= 0 {
		n = 2
	}
	args := []string{"--spec-type", "draft-mtp", "--spec-draft-n-max", strconv.Itoa(n)}
	if !isMTPFile(modelPath) {
		if head := mtpHeadFor(modelPath); head != "" {
			args = append(args, "--spec-draft-model", head)
		}
	}
	// The MTP draft context keeps its OWN KV cache (f16 by default) that scales with
	// the full window -- at large windows it allocates last and OOMs the fullest card
	// (measured: a 768 MiB f16 draft cache at 131K killed the load on a 16+12 GB
	// pair). Quantize it like the main cache: drafts are verified by the main model,
	// so a lighter draft cache only nudges acceptance, never output quality.
	if cfg.Performance.FlashAttn {
		if ct := EffectiveCacheType(cfg, hw, modelPath, FileMB(modelPath), WillOffloadExperts(cfg, hw, modelPath)); ct != "" && ct != "f16" {
			args = append(args, "--spec-draft-type-k", ct, "--spec-draft-type-v", ct)
		}
	}
	return args
}

// NextLadderRung is the smallest standard context rung strictly above ctx, capped
// at the ceiling.
func NextLadderRung(ctx int) int {
	for _, s := range []int{49152, 65536, 98304, 131072, 196608, 262144} {
		if s > ctx {
			return s
		}
	}
	return ctxCeil
}

// PlanForModel reports the context window and MoE-offload decision winc would use
// for a model file of the given on-disk size in MB (0 = unknown). For diagnostics
// (winc detect). cpuMoe is "" (none / full GPU), "all" (--cpu-moe), or a layer count.
func PlanForModel(cfg *config.Config, hw platform.Hardware, modelFile string, modelMB int) (ctx int, cpuMoe string) {
	cpuMoe = resolveCPUMoE(cfg, hw, modelFile, modelMB, GpuLayers(cfg, hw))
	ctx = ResolveContext(cfg, hw, modelFile, modelMB, cpuMoe == "all")
	return ctx, cpuMoe
}

// Multi-GPU layer placement is deliberately left to the engine: llama-server's
// device-memory fit spreads layers across every CUDA device by its free VRAM at
// load time. Passing an explicit --tensor-split DISABLES that fit ("already set
// by user, abort") and a hand-computed split can overpack a card once the KV
// cache, compute buffers, and MTP context land on it (verified: cublasCreate
// OOM on device 1). winc's job is only to budget for the combined VRAM.

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// FamilySamplingArgs returns the model authors' recommended sampling for a known model
// family (ANY size, main or worker) used as a tool-calling agent. Correct sampling
// materially affects tool-call reliability and avoids repetition loops; running a model at
// the wrong temperature (e.g. Gemma wants 1.0, not llama.cpp's default) degrades it. Returns
// nil for families with no profile, leaving llama.cpp's defaults. Client-sent params still
// override these, so they set mainly what Claude Code omits (top_k / min_p / presence).
func FamilySamplingArgs(modelPath string) []string {
	name := strings.ToLower(filepath.Base(modelPath))
	switch {
	case strings.Contains(name, "qwen"):
		// Qwen3 / Qwen3.5 official: temp 0.7 / top-p 0.8 / top-k 20 / min-p 0; presence
		// penalty 1.0 guards the tiny variants against endless repetition.
		return []string{"--temp", "0.7", "--top-p", "0.8", "--top-k", "20", "--min-p", "0.0", "--presence-penalty", "1.0"}
	case strings.Contains(name, "gemma"):
		// Gemma 3 / 4 recommended: temp 1.0 / top-k 64 / top-p 0.95.
		return []string{"--temp", "1.0", "--top-k", "64", "--top-p", "0.95", "--min-p", "0.0"}
	default:
		return nil
	}
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
	ctxFloor = 32768  // enough for Claude Code's system prompt + tools + headroom
	ctxCeil  = 262144 // every 2026 catalog model is natively >=256K; the load ladder protects the rest
)

// StarvedCtxTokens is the auto window below which the KV cache downshifts to q4_0
// (cache_type = "auto"): halving the KV bytes roughly doubles a starved window,
// exactly where it matters most (low-end cards, tight fits).
const StarvedCtxTokens = 65536

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

// mtpCtxReserveMB is the extra VRAM an active MTP draft context occupies (the
// engine reports ~865 MiB for the 26B/35B heads; rounded up). Budgeted into the
// sizing math so the auto-context never overcommits VRAM and pushes model layers
// off the GPU.
const mtpCtxReserveMB = 1024

// mtpActive reports whether MTP will engage for this model: baked-in heads or a
// downloaded external head, config permits, and the backend isn't Metal. The
// engine-support probe is separate (MTPArgs) -- this is the sizing-level check.
func mtpActive(cfg *config.Config, modelPath string) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.Performance.Mtp), "off") {
		return false
	}
	if !isMTPFile(modelPath) && mtpHeadFor(modelPath) == "" {
		return false
	}
	// draft-mtp is unstable on the Metal backend (crashes during inference -> the
	// agent retries forever). CUDA/Vulkan/CPU keep MTP.
	return CurrentBackend() != "metal"
}

// mtpReserveMB is the MTP draft-context allowance for a launch: nonzero only when
// MTP will actually engage for this model.
func mtpReserveMB(cfg *config.Config, modelPath string) int {
	if mtpActive(cfg, modelPath) {
		return mtpCtxReserveMB
	}
	return 0
}

// fullyFitsGPU reports whether the model, the per-GPU reserves, the MTP draft
// context (when active), and at least a minimal KV cache all fit the combined
// VRAM. When true winc forces -ngl 99: the engine's own device fit is
// conservative and can spill a layer to the CPU on a tight-but-sufficient fit --
// and on a MoE even one CPU-resident layer drags every token through a slow CPU
// expert pass, competing with the team's CPU workers. The context ladder still
// protects an overcommit: if the forced load fails, the launcher steps the
// context down and retries. Unified (Apple) memory keeps its existing behavior.
func fullyFitsGPU(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB int) bool {
	if hw.Unified || hw.VRAMMB <= 0 || modelMB <= 0 {
		return false
	}
	return hw.VRAMMB-modelMB-gpuReserveMB(hw)-mtpReserveMB(cfg, modelPath) >= minKVHeadroomMB
}

// EffectiveCacheType resolves cache_type = "auto": q8_0 normally, downshifted to
// q4_0 when the q8-sized window would be starved (< StarvedCtxTokens). Explicit
// values pass through untouched. Quantized KV needs flash attention; without it
// the cache is f16 regardless, so auto stays on q8_0 there (the flags are skipped).
func EffectiveCacheType(cfg *config.Config, hw platform.Hardware, modelPath string, modelFileMB int, expertsOffloaded bool) string {
	ct := strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType))
	if ct != "" && ct != "auto" {
		return ct
	}
	if !cfg.Performance.FlashAttn || modelFileMB <= 0 {
		return "q8_0" // no flash-attn (cache is f16 anyway) or unknown size -> never downshift
	}
	if resolveContextFor(cfg, hw, "q8_0", modelPath, modelFileMB, expertsOffloaded) < StarvedCtxTokens {
		return "q4_0"
	}
	return "q8_0"
}

// ResolveContext picks a liberal context window: the configured value, or (auto)
// the largest that should fit free VRAM after the model, clamped to a safe range.
// The launcher verifies the choice actually loads and falls back if not. When the
// model's experts are offloaded to RAM (expertsOffloaded), most of its VRAM is free
// for KV, so we aim at the ceiling and let the launcher's ladder settle the max.
func ResolveContext(cfg *config.Config, hw platform.Hardware, modelPath string, modelFileMB int, expertsOffloaded bool) int {
	return resolveContextFor(cfg, hw, EffectiveCacheType(cfg, hw, modelPath, modelFileMB, expertsOffloaded), modelPath, modelFileMB, expertsOffloaded)
}

// resolveContextFor is ResolveContext with the KV cache type pinned (the "auto"
// resolution needs to size the q8 window without recursing).
func resolveContextFor(cfg *config.Config, hw platform.Hardware, cacheType, modelPath string, modelFileMB int, expertsOffloaded bool) int {
	if cfg.Performance.Context != "auto" && cfg.Performance.Context != "" {
		return atoiOr(cfg.Performance.Context, ctxFloor)
	}
	if GpuLayers(cfg, hw) == 0 || hw.VRAMMB <= 0 {
		return ctxFloor
	}
	if expertsOffloaded {
		return ctxCeil // experts in RAM -> lots of VRAM free; ladder fits the largest that loads
	}
	if modelFileMB <= 0 {
		return ctxFloor
	}
	// Reserve compute buffer(s), the MTP draft context when one will load, + safety.
	free := hw.VRAMMB - modelFileMB - gpuReserveMB(hw) - mtpReserveMB(cfg, modelPath)
	if free <= 0 {
		return ctxFloor
	}
	// Bytes/token depends on the KV cache type, so a smaller cache (q4) fits a
	// proportionally larger window. Default q8_0 keeps the original factor (64).
	toks := free * kvCtxFactor(cacheType, cfg.Performance.FlashAttn)
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
	steps := []int{target, 196608, 131072, 98304, 65536, 49152, 32768, 24576, 16384}
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
	// Some 2026 templates (Qwen3.5) carry a system-position guard that breaks llama.cpp's
	// tool-call parser generation -> 400 on every request. Override with a patched copy.
	args = append(args, ChatTemplateArgs(modelPath)...)

	ngl := GpuLayers(cfg, hw)
	// GPU placement policy, head-first:
	//  - MoE expert offload: all layers on the GPU (-ngl 99), expert weights in RAM,
	//    so a MoE bigger than VRAM still runs fast (only activations cross PCIe).
	//  - Explicit gpu_layers: the user's number wins.
	//  - Model fully fits VRAM: force -ngl 99 so the engine's conservative device
	//    fit can't spill a layer to the CPU (the CPU belongs to the team workers).
	//  - Otherwise (partial fit, dense): omit -ngl and let the engine fit layers.
	switch cpuMoe := resolveCPUMoE(cfg, hw, modelPath, FileMB(modelPath), ngl); {
	case cpuMoe == "all":
		args = append(args, "-ngl", "99", "--cpu-moe")
	case cpuMoe != "":
		args = append(args, "-ngl", "99", "--n-cpu-moe", cpuMoe)
	case cfg.Performance.GpuLayers != "auto" && cfg.Performance.GpuLayers != "":
		args = append(args, "-ngl", strconv.Itoa(ngl))
	case fullyFitsGPU(cfg, hw, modelPath, FileMB(modelPath)):
		args = append(args, "-ngl", "99")
	}

	if ctx <= 0 {
		ctx = ResolveContext(cfg, hw, modelPath, FileMB(modelPath), WillOffloadExperts(cfg, hw, modelPath))
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
		if ct := EffectiveCacheType(cfg, hw, modelPath, FileMB(modelPath), WillOffloadExperts(cfg, hw, modelPath)); ct != "" && ct != "f16" {
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

	// Family-correct sampling (Qwen / Gemma published values) for tool-call reliability;
	// no-op for unknown families. Applies to every model -- main, single, and workers --
	// not just the small ones. Before ExtraServerArgs so a user's own flags win.
	args = append(args, FamilySamplingArgs(modelPath)...)

	// Advanced escape hatch: any extra llama-server flags, verbatim.
	args = append(args, cfg.Performance.ExtraServerArgs...)
	return args
}
