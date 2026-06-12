package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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
// allocates its own compute buffer. Compute buffers scale with the MODEL, not the
// card: the flat 1536 MB calibrated on 20+ GB models ate a 4 GB card's entire
// non-model VRAM and collapsed its sizing to the floor (a 4B's real compute
// buffer is ~300 MB). Small models reserve proportionally less; >=8 GB models
// keep the calibrated 1536 exactly. Unknown size stays conservative.
func gpuReserveMB(hw platform.Hardware, modelMB int) int {
	n := len(hw.GPUs)
	if n < 1 {
		n = 1
	}
	base := 1536
	if modelMB > 0 && 512+modelMB/8 < base {
		base = 512 + modelMB/8
	}
	return base + 768*(n-1)
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
	if hw.VRAMMB-modelMB-gpuReserveMB(hw, modelMB) < minKVHeadroomMB {
		return "all"
	}
	return ""
}

// WillOffloadExperts reports whether winc will move this model's MoE experts to RAM
// (--cpu-moe) -- which frees most of the model's VRAM for a much larger KV cache.
func WillOffloadExperts(cfg *config.Config, hw platform.Hardware, modelPath string) bool {
	return resolveCPUMoE(cfg, hw, modelPath, FileMB(modelPath), GpuLayers(cfg, hw)) == "all"
}

// ForcedFullGPU reports whether this launch pins the model fully onto the GPU
// (the fullyFitsGPU -ngl 99 policy) -- the loads whose VRAM residency the
// launcher verifies after each attempt, because a pinned load that exceeds free
// dedicated memory can be silently satisfied from shared system memory instead
// of failing. Explicit gpu_layers, unified memory, expert offload, and partial
// fits run as written and are never gated.
func ForcedFullGPU(cfg *config.Config, hw platform.Hardware, modelPath string) bool {
	return forcedFullGPUAt(cfg, hw, modelPath, FileMB(modelPath))
}

// forcedFullGPUAt is ForcedFullGPU with the model size supplied directly --
// testable without multi-GB fixture files (POSIX filesystems keep truncated
// fixtures sparse, but NTFS allocates them for real, which exhausted the
// Windows CI runner's disk).
func forcedFullGPUAt(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB int) bool {
	if cfg.Performance.FFNSpill > 0 {
		// The FFN-spill placement pins -ngl 99 with the spilled blocks excused:
		// it is set only by winc's own bottom-target stage, whose budget math
		// already validated the resident set -- and exactly like any other pin,
		// an over-budget load can silently land in shared system memory, so the
		// gate must cover it (against the REDUCED resident size).
		return true
	}
	if cfg.Performance.GpuLayers != "auto" && cfg.Performance.GpuLayers != "" {
		return false
	}
	if resolveCPUMoE(cfg, hw, modelPath, modelMB, GpuLayers(cfg, hw)) != "" {
		return false
	}
	return fullyFitsGPU(cfg, hw, modelPath, modelMB)
}

// mainEscalationHeadroomMB is the free VRAM (after the main model + compute buffer)
// required before winc will let subagents escalate onto the main GPU model. Below this,
// escalation tops out at the CPU worker so the orchestrator stays responsive and the KV
// cache isn't starved by extra concurrent sequences.
const mainEscalationHeadroomMB = 6000

// MainEscalationOK reports whether the main GPU model has enough spare VRAM to also
// serve escalated subagents concurrently. They share the engine's unified KV pool
// (no --parallel split -- the head keeps its WHOLE window; concurrency costs pool
// room only while a subagent actually runs), so the only question is headroom.
// False when there's no GPU, when experts are offloaded to RAM (the main model is
// already compute-compromised), or when free VRAM after the model is below the
// headroom threshold -- in those cases escalation stops at the CPU worker.
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
	return hw.VRAMMB-mb-gpuReserveMB(hw, mb) >= mainEscalationHeadroomMB
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
			k, v := SplitKV(ct)
			args = append(args, "--spec-draft-type-k", k, "--spec-draft-type-v", v)
		}
	}
	return args
}

// SplitMeasured reports whether every detected GPU carries a measured solo
// decode speed -- the precondition for the bandwidth-weighted tensor split.
func SplitMeasured(hw platform.Hardware) bool {
	if len(hw.GPUs) < 2 {
		return false
	}
	for _, g := range hw.GPUs {
		if g.SpeedTPS <= 0 {
			return false
		}
	}
	return true
}

// TensorSplitArgs returns an explicit --tensor-split for a forced-full-GPU
// multi-GPU load -- nil when it doesn't apply (single GPU, unmeasured or
// unprobed cards, or budgets that can't hold the footprint; the engine default
// then stands). The pinned -ngl aborts the engine's own device fit, so
// placement falls to the free-VRAM-ratio default, which BALANCES the cards --
// but decode on a layer split is ADDITIVE (t = sum of bytes_i / bandwidth_i),
// so the optimum packs the FASTEST card to its budget and hands the slow card
// only the remainder. Measured on a 5070Ti+3060 pair (460 vs 210 tok/s solo):
// the balanced default left ~2.5 GB of the fast card idle while the slow card
// gated every token. The placement gate still verifies the result; a bad
// split steps down exactly like any failed rung.
func TensorSplitArgs(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB, ctx int, cacheType string) []string {
	n := len(hw.GPUs)
	if n < 2 || modelMB <= 0 || ctx <= 0 || cfg.Performance.NoTensorSplit || !SplitMeasured(hw) {
		return nil
	}
	totalFree := 0
	for _, g := range hw.GPUs {
		if g.FreeMB <= 0 {
			return nil
		}
		totalFree += g.FreeMB
	}
	kvMB := 0
	if f := kvCtxFactor(cacheType, cfg.Performance.FlashAttn); f > 0 {
		kvMB = ctx / f
	}
	footprint := float64(modelMB + kvMB + mtpReserveMB(cfg, modelPath))
	totalReserve := gpuReserveMB(hw, modelMB)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return hw.GPUs[order[a]].SpeedTPS > hw.GPUs[order[b]].SpeedTPS })
	fracs := make([]float64, n)
	left := footprint
	for _, i := range order {
		// Per-card margin: this card's share of the calibrated reserve (compute
		// buffers grow with the layers it hosts) plus a hard 1 GB floor -- the
		// first cut packed the fast card to ~300 MB of slack and a load that
		// fit under the balanced default OOM'd under the "optimal" split.
		reserve := totalReserve * hw.GPUs[i].FreeMB / totalFree
		if reserve < 1024 {
			reserve = 1024
		}
		b := float64(hw.GPUs[i].FreeMB - reserve)
		if b < 0 {
			b = 0
		}
		take := b
		if take > left {
			take = left
		}
		fracs[i] = take
		left -= take
	}
	if left > 0.5 {
		return nil // footprint exceeds the budgets -> ladder/default handles it
	}
	parts := make([]string, n)
	for i, f := range fracs {
		parts[i] = strconv.FormatFloat(f/footprint, 'f', 3, 64)
	}
	return []string{"--tensor-split", strings.Join(parts, ",")}
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

// GpuLayersEngine is the winc-internal gpu_layers sentinel for the bottom-target
// spill rescue: ServerArgs omits -ngl entirely so the engine's device fit places
// the layers (spilling to RAM as needed). Everything else treats it like a
// GPU-offloaded launch (flash attention, batch sizes, KV quantization).
const GpuLayersEngine = "engine"

// GpuLayers resolves the -ngl value from config + hardware.
func GpuLayers(cfg *config.Config, hw platform.Hardware) int {
	if cfg.Performance.GpuLayers == "auto" || cfg.Performance.GpuLayers == "" || cfg.Performance.GpuLayers == GpuLayersEngine {
		if hw.GPUVendor != "" && hw.GPUVendor != "none" {
			return 99
		}
		return 0
	}
	return atoiOr(cfg.Performance.GpuLayers, 99)
}

const (
	ctxFloor = 32768  // last-resort ladder bottom; enough to boot an agent at all
	ctxCeil  = 262144 // every 2026 catalog model is natively >=256K; the load ladder protects the rest

	// BottomCtxTokens is the UNIVERSAL bottom target: ~64K of usable working
	// context on top of Claude Code's fixed overhead (~24k system prompt +
	// tools) and the compaction reserve (~8-12k) -- ~100k total. Auto sizing
	// aims at the ceiling and settles at the largest window that loads
	// HEALTHY; but it never settles below this bottom while a slower path
	// exists: when full-GPU residency can't reach it, the launcher retries
	// with the engine's device placement (layers spill to RAM) at exactly
	// this window. A slower usable window beats a fast cramped one -- the
	// decode report states the price.
	BottomCtxTokens = 98304
)

// ParallelSlots reads --parallel N from the extra server args (team mode adds it
// when subagents may escalate to the head): the per-agent window is the total
// divided across the slots, so sizing targets scale with it.
func ParallelSlots(cfg *config.Config) int {
	ex := cfg.Performance.ExtraServerArgs
	for i, a := range ex {
		if a == "--parallel" && i+1 < len(ex) {
			if n, err := strconv.Atoi(ex[i+1]); err == nil && n > 1 {
				return n
			}
		}
	}
	return 1
}

// StarvedCtxTokens is the auto window below which the KV cache downshifts to q4_0
// (cache_type = "auto"): halving the KV bytes roughly doubles a starved window,
// exactly where it matters most (low-end cards, tight fits).
const StarvedCtxTokens = 65536

// kvCtxFactor is the auto-context multiplier (tokens per free MB of VRAM) for a KV
// cache type. q8_0 (~16 KB/token) is the baseline 64; f16 doubles the bytes (so
// halves the tokens), q4 halves the bytes (so doubles the tokens). Conservative.
// An asymmetric "k/v" pair combines harmonically (bytes add per side). KV
// quantization needs flash attention -- without it the cache is f16 regardless.
func kvCtxFactor(cacheType string, flashAttn bool) int {
	if !flashAttn {
		return 32 // f16 K+V
	}
	if k, v := SplitKV(strings.ToLower(strings.TrimSpace(cacheType))); k != v {
		fk, fv := kvSideFactor(k), kvSideFactor(v)
		return 2 * fk * fv / (fk + fv)
	}
	return kvSideFactor(strings.ToLower(strings.TrimSpace(cacheType)))
}

func kvSideFactor(cacheType string) int {
	switch cacheType {
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
	return hw.VRAMMB-modelMB-gpuReserveMB(hw, modelMB)-mtpReserveMB(cfg, modelPath) >= minKVHeadroomMB
}

// mainBlocks is the transformer block count EXCLUDING an MTP head's extra block
// (its tensors only load for speculative decoding, and the FFN-spill placement
// always runs with the draft off).
func mainBlocks(modelPath string) int {
	b := BlockCount(modelPath)
	if b > 1 && isMTPFile(modelPath) {
		b--
	}
	return b
}

// FFNSpillArgs builds the tensor override that parks the LAST k blocks'
// feed-forward weights in system RAM while -ngl 99 keeps everything else --
// every attention/SSM tensor and the entire KV cache -- GPU-resident. Dense
// decode cost is linear in the spilled bytes and (measured) DEPTH-STABLE,
// where whole-layer spill also drags those layers' attention and KV through
// RAM and decays as the context fills. Block indices are spelled out
// explicitly: unambiguous to read in a process list and immune to regex
// edge cases.
func FFNSpillArgs(modelPath string, k int) []string {
	main := mainBlocks(modelPath)
	if k <= 0 || main <= 0 {
		return nil
	}
	if k > main {
		k = main
	}
	idx := make([]string, 0, k)
	for b := main - k; b < main; b++ {
		idx = append(idx, strconv.Itoa(b))
	}
	return []string{"-ot", `blk\.(` + strings.Join(idx, "|") + `)\.ffn_.*=CPU`}
}

// FFNSpillPlan answers "how many trailing blocks' FFN weights must move to RAM
// for ctx tokens of KV to fit" from the same budget terms the auto sizing uses.
// Returns (k, mainBlocks):
//
//	k == 0          -> spill can't help here (budget already fits, not a dense
//	                   GPU launch, or the model's FFN layout is unreadable)
//	0 < k <= blocks -> spill k blocks' FFN (includes a +1 block safety margin)
//	k > blocks      -> even every FFN in RAM can't afford ctx; try smaller
//
// MoE models never plan an FFN spill -- expert offload (--cpu-moe) is their
// (cheaper) version of exactly this trade. The caller supplies a config whose
// MTP is already resolved (the spill stage runs with the draft off).
func FFNSpillPlan(cfg *config.Config, hw platform.Hardware, modelPath string, ctx int) (k, blocks int) {
	if hw.Unified || hw.VRAMMB <= 0 || len(hw.GPUs) == 0 {
		return 0, 0
	}
	if cfg.Performance.GpuLayers != "auto" && cfg.Performance.GpuLayers != "" {
		return 0, 0
	}
	if isMoEFile(modelPath) {
		return 0, 0
	}
	modelMB := FileMB(modelPath)
	layerMB := FFNLayerMB(modelPath)
	main := mainBlocks(modelPath)
	if modelMB <= 0 || layerMB <= 0 || main <= 0 {
		return 0, 0
	}
	ct := EffectiveCacheType(cfg, hw, modelPath, modelMB, false)
	needKVMB := ctx / kvCtxFactor(ct, cfg.Performance.FlashAttn)
	haveMB := hw.VRAMMB - modelMB - gpuReserveMB(hw, modelMB) - mtpReserveMB(cfg, modelPath)
	deficit := needKVMB - haveMB
	if deficit <= 0 {
		return 0, main
	}
	return (deficit+layerMB-1)/layerMB + 1, main
}

// FFNSpillMB is the weight bytes (MB) a k-block FFN spill moves off the GPU.
func FFNSpillMB(modelPath string, k int) int {
	if main := mainBlocks(modelPath); k > main {
		k = main
	}
	if k <= 0 {
		return 0
	}
	return k * FFNLayerMB(modelPath)
}

// EffectiveCacheType resolves cache_type = "auto": q8_0 normally, downshifted to
// the ASYMMETRIC "q8_0/q4_0" (key cache / value cache) when the q8-sized window
// would be starved (< StarvedCtxTokens). Keys are far more sensitive to
// quantization than values (4-bit keys measure ~+10% perplexity -- past the
// usefulness line for coding -- while 4-bit values are near-lossless), so the
// downshift halves only the value side: ~1.3x the window at minimal quality cost.
// Explicit values pass through untouched, including an explicit "k/v" pair.
// Quantized KV needs flash attention; without it the cache is f16 regardless.
func EffectiveCacheType(cfg *config.Config, hw platform.Hardware, modelPath string, modelFileMB int, expertsOffloaded bool) string {
	ct := strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType))
	if ct != "" && ct != "auto" {
		return ct
	}
	if !cfg.Performance.FlashAttn || modelFileMB <= 0 {
		return "q8_0" // no flash-attn (cache is f16 anyway) or unknown size -> never downshift
	}
	// Starvation is judged on the RAW full-GPU estimate, not the bottom-bumped
	// target: the bump reports a window the KV budget can't actually hold, which
	// would hide starvation from the exact cards the downshift exists for.
	if !expertsOffloaded && rawCtxTokens(cfg, hw, "q8_0", modelPath, modelFileMB) < StarvedCtxTokens {
		return "q8_0/q4_0"
	}
	return "q8_0"
}

// SplitKV splits a cache-type value into its key-cache and value-cache types: a
// plain type applies to both sides; "k/v" sets them separately.
func SplitKV(ct string) (k, v string) {
	if i := strings.IndexByte(ct, '/'); i >= 0 {
		return ct[:i], ct[i+1:]
	}
	return ct, ct
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
	mode := strings.ToLower(strings.TrimSpace(cfg.Performance.Context))
	switch mode {
	case "", "auto", "optimal":
	default:
		return atoiOr(cfg.Performance.Context, ctxFloor)
	}
	if GpuLayers(cfg, hw) == 0 || hw.VRAMMB <= 0 {
		return ctxFloor
	}
	// One universal aim: the 262144 ceiling when it loads healthy ("optimal" and
	// "auto" are the same policy now); the ladder, the fit oracle, and the
	// placement gate settle the largest TRUE window from there.
	limit := ctxCeil
	if expertsOffloaded {
		return limit // experts in RAM -> lots of VRAM free; ladder fits the largest that loads
	}
	if modelFileMB <= 0 {
		return ctxFloor
	}
	toks := rawCtxTokens(cfg, hw, cacheType, modelPath, modelFileMB)
	if toks < BottomCtxTokens {
		// Full-GPU sizing can't reach the bottom target (a tiny card, or a model
		// near the card's size). Aim at the bottom and let the engine's device
		// fit spill layers to RAM -- the ladder still steps down if even that
		// won't load, and the decode report states the price.
		if limit < BottomCtxTokens {
			return limit
		}
		return BottomCtxTokens
	}
	if toks > limit {
		return limit
	}
	return toks
}

// rawCtxTokens is the UNBUMPED full-GPU window estimate: what the KV budget
// alone affords, before the bottom-target policy raises the aim. The starved
// KV downshift must read THIS value -- the bottom bump would otherwise hide
// starvation (a 40k raw window reported as the 98k target reads as "ample",
// the asym downshift never fires, and the exact cards it exists for lose it).
func rawCtxTokens(cfg *config.Config, hw platform.Hardware, cacheType, modelPath string, modelFileMB int) int {
	// Reserve compute buffer(s), the MTP draft context when one will load, + safety.
	free := hw.VRAMMB - modelFileMB - gpuReserveMB(hw, modelFileMB) - mtpReserveMB(cfg, modelPath)
	if free <= 0 {
		return 0
	}
	// Bytes/token depends on the KV cache type, so a smaller cache (q4) fits a
	// proportionally larger window. Default q8_0 keeps the original factor (64).
	toks := free * kvCtxFactor(cacheType, cfg.Performance.FlashAttn)
	return (toks / 8192) * 8192
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

	modelMB := FileMB(modelPath)
	expertsOff := WillOffloadExperts(cfg, hw, modelPath)
	ngl := GpuLayers(cfg, hw)
	if ctx <= 0 {
		ctx = ResolveContext(cfg, hw, modelPath, modelMB, expertsOff)
	}
	// GPU placement policy, head-first:
	//  - MoE expert offload: all layers on the GPU (-ngl 99), expert weights in RAM,
	//    so a MoE bigger than VRAM still runs fast (only activations cross PCIe).
	//  - gpu_layers = "engine" (winc-internal, the bottom-target spill rescue):
	//    omit -ngl so the engine's device fit places layers -- a deliberate spill
	//    for a window the resident set can't hold.
	//  - Explicit gpu_layers: the user's number wins.
	//  - Model fully fits VRAM: force -ngl 99 so the engine's conservative device
	//    fit can't spill a layer to the CPU (the CPU belongs to the team workers) --
	//    and on measured multi-GPU machines, place the layers by BANDWIDTH, not
	//    balance (TensorSplitArgs): the pinned -ngl already forfeited the engine's
	//    own fit, and the free-ratio default leaves the fast card idle.
	//  - Otherwise (partial fit, dense): omit -ngl and let the engine fit layers.
	switch cpuMoe := resolveCPUMoE(cfg, hw, modelPath, modelMB, ngl); {
	case cfg.Performance.FFNSpill > 0:
		// Dense FFN spill (winc-internal): pin everything resident EXCEPT the
		// chosen blocks' feed-forward weights. No tensor-split here -- the
		// per-card budget math doesn't model the FFN holes; the engine's own
		// balance places what remains.
		args = append(args, "-ngl", "99")
		args = append(args, FFNSpillArgs(modelPath, cfg.Performance.FFNSpill)...)
	case cpuMoe == "all":
		args = append(args, "-ngl", "99", "--cpu-moe")
	case cpuMoe != "":
		args = append(args, "-ngl", "99", "--n-cpu-moe", cpuMoe)
	case cfg.Performance.GpuLayers == GpuLayersEngine:
		// engine placement: no -ngl
	case cfg.Performance.GpuLayers != "auto" && cfg.Performance.GpuLayers != "":
		args = append(args, "-ngl", strconv.Itoa(ngl))
	case fullyFitsGPU(cfg, hw, modelPath, modelMB):
		args = append(args, "-ngl", "99")
		args = append(args, TensorSplitArgs(cfg, hw, modelPath, modelMB, ctx, EffectiveCacheType(cfg, hw, modelPath, modelMB, expertsOff))...)
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
		if ct := EffectiveCacheType(cfg, hw, modelPath, modelMB, expertsOff); ct != "" && ct != "f16" {
			k, v := SplitKV(ct)
			args = append(args, "--cache-type-k", k, "--cache-type-v", v)
		}
	}

	if cfg.Performance.Threads != "auto" && cfg.Performance.Threads != "" {
		args = append(args, "-t", cfg.Performance.Threads)
	}

	// Reasoning: static modes set server flags; adaptive runs in "auto" and lets
	// winc-router cap the budget per request.
	switch cfg.Reasoning.Mode {
	case "off":
		// Template-level disable, NOT --reasoning-budget 0: measured on Qwen3.5
		// (2B/4B), budget-0 still routes every generated token into the thinking
		// channel -- the content comes back EMPTY with the whole max_tokens spent.
		// --reasoning off renders the template without a thinking turn and the
		// answer arrives in content, full speed.
		args = append(args, "--reasoning", "off")
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
