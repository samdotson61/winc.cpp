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
	"winc/internal/ui"
)

// moeNamePat matches the MoE naming convention: an active-param tag like "A3B" /
// "A22B" / "A1.4B", or "moe"/"gpt-oss" in the filename.
var moeNamePat = regexp.MustCompile(`(?i)(^|[-_.])a\d+(\.\d+)?b([-_.]|$)|moe|gpt-oss`)

// isMoEFile guesses whether a GGUF is a Mixture-of-Experts model from its name.
// A guess is all a name can give: plenty of real MoEs carry no active-param tag
// (Mixtral-8x7B, DeepSeek-V2-Lite, grok-1), and any renamed file loses the tag
// entirely. Use IsMoE instead wherever a real file is on hand.
func isMoEFile(path string) bool { return moeNamePat.MatchString(filepath.Base(path)) }

// IsMoE reports whether a GGUF is a Mixture-of-Experts model, preferring the
// authoritative "<arch>.expert_count" in the file's metadata and falling back to
// the filename convention only when that metadata cannot be read -- which is the
// normal case for a catalogue entry naming a model that isn't downloaded yet.
// Getting this wrong is expensive: a MoE misread as dense never gets its experts
// offloaded, so a VRAM-tight machine is left with a floor-level context or an OOM.
func IsMoE(path string) bool {
	switch n := ExpertCount(path); {
	case n > 0:
		return true
	case n == 0:
		return false // metadata read cleanly and says dense
	default:
		return isMoEFile(path) // metadata unavailable -> fall back to the name
	}
}

// draftWarned tracks target|draft pairs already reported as tokenizer-incompatible.
var draftWarned sync.Map

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
	if ngl == 0 || hw.VRAMMB <= 0 || modelMB <= 0 || !IsMoE(modelPath) {
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

// moePackSafetyMB is extra VRAM held back by the expert-packing budget beyond
// the standard reserves. Packed loads are pinned (-ngl 99) and an over-budget
// pin is exactly what the WDDM driver satisfies silently from shared system
// memory -- the placement gate catches that, but a margin avoids burning a
// ladder rung to find out.
const moePackSafetyMB = 512

// MoEPackPlan computes the expert-packing placement for a MoE launch that
// auto-offloads: instead of --cpu-moe (EVERY layer's routed experts to RAM,
// stranding whatever VRAM the window doesn't use), keep as many layers'
// experts as fit ON the GPU and offload only the rest via --n-cpu-moe.
// The WINDOW is budgeted first -- packing never shrinks the context; experts
// take only the leftover. Returns (cpuLayers, cpuSpillMB, true) when the plan
// applies: cfg is auto, auto-offload fired, expert sizing is readable, and at
// least one layer's experts fit in the leftover. ok=false means launch as
// before (--cpu-moe). Explicit cpu_moe settings ("on"/"all"/a count) are the
// user's word and never repacked.
func MoEPackPlan(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB, ctx int) (cpuLayers, cpuSpillMB int, ok bool) {
	switch strings.ToLower(strings.TrimSpace(cfg.Performance.CpuMoe)) {
	case "", "auto":
	default:
		return 0, 0, false
	}
	if ctx <= 0 || modelMB <= 0 {
		return 0, 0, false
	}
	if resolveCPUMoE(cfg, hw, modelPath, modelMB, GpuLayers(cfg, hw)) != "all" {
		return 0, 0, false
	}
	expTotalMB, expLayers := MoEExpertStats(modelPath)
	if expTotalMB <= 0 || expLayers <= 0 {
		return 0, 0, false
	}
	perLayerMB := expTotalMB / expLayers
	if perLayerMB <= 0 {
		return 0, 0, false
	}
	// Resident base = everything --cpu-moe keeps on the GPU anyway: attention,
	// shared experts, embeddings, output. KV is sized for the ALREADY-DECIDED
	// window (the offloaded-experts context path), so packing can't shrink it.
	baseMB := modelMB - expTotalMB
	kvMB := 0
	if f := kvCtxFactor(EffectiveCacheType(cfg, hw, modelPath, modelMB, true), cfg.Performance.FlashAttn); f > 0 {
		kvMB = (ctx + f - 1) / f
	}
	free := hw.VRAMMB - baseMB - kvMB - specReserveMB(cfg, modelPath) - gpuReserveMB(hw, modelMB) - moePackSafetyMB
	gpuLayers := free / perLayerMB
	if gpuLayers < 1 {
		return 0, 0, false
	}
	if gpuLayers >= expLayers {
		gpuLayers = expLayers - 1 // stay in the offload family the sizing assumed
	}
	cpuLayers = expLayers - gpuLayers
	return cpuLayers, cpuLayers * perLayerMB, true
}

// MoEPackSpillMB is the CPU-side expert bytes (MB) of the active pack plan, or
// 0 when packing doesn't apply. The placement gate subtracts this from the
// expected resident set exactly like a dense FFN spill.
func MoEPackSpillMB(cfg *config.Config, hw platform.Hardware, modelPath string, modelMB, ctx int) int {
	if _, spill, ok := MoEPackPlan(cfg, hw, modelPath, modelMB, ctx); ok {
		return spill
	}
	return 0
}

// ForcedFullGPUCtx is ForcedFullGPU with the resolved window supplied: a
// packed-MoE launch pins -ngl 99 with only PART of the experts resident, so it
// is exactly the load class the placement gate exists for (a silently
// sysmem-satisfied pin) and must be gated against its reduced resident set,
// like a dense FFN spill. Window-independent callers keep ForcedFullGPU.
func ForcedFullGPUCtx(cfg *config.Config, hw platform.Hardware, modelPath string, ctx int) bool {
	if _, _, ok := MoEPackPlan(cfg, hw, modelPath, FileMB(modelPath), ctx); ok {
		return true
	}
	return ForcedFullGPU(cfg, hw, modelPath)
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
	mtpProbe   = map[string]string{} // bin path -> its --help text
)

// serverSupportsMTP / serverSupportsNgram report whether a llama-server binary
// understands the respective --spec-type. The --help text is cached per binary
// path (--help is cheap but we may ask several times per launch).
func serverSupportsMTP(bin string) bool    { return serverHelpContains(bin, "draft-mtp") }
func serverSupportsNgram(bin string) bool  { return serverHelpContains(bin, "ngram-simple") }
func serverSupportsDFlash(bin string) bool { return serverHelpContains(bin, "draft-dflash") }

func serverHelpContains(bin, needle string) bool {
	mtpProbeMu.Lock()
	defer mtpProbeMu.Unlock()
	help, ok := mtpProbe[bin]
	if !ok {
		cmd := exec.Command(bin, "--help")
		cmd.Env = mtpProbeEnv(bin)
		out, _ := cmd.CombinedOutput()
		help = string(out)
		mtpProbe[bin] = help
	}
	return strings.Contains(help, needle)
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

// --cache-reuse was dropped in v1.27.0: the v1.25.0 journal-era A/B measured NO
// gain from it (the request pipeline is already prefix-cache-optimal), and a
// flag that does nothing here is one more engine interaction surface. Users who
// want it back have extra_server_args.

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

// SpecArgs returns the full speculative-decoding flags for a launch: the MTP
// flags (when includeMTP and the model has heads) composed with the model-free
// ngram-simple drafter (when the backend and engine support it). llama-server
// takes ONE --spec-type flag with a comma list, so the two are merged here
// rather than appended separately (a repeated flag would drop one of them).
//
// ngram-simple drafts from n-grams already seen in the prompt + generation --
// no draft model, no VRAM, no artifacts. MEASURED (b10298, 5070 Ti, temp 0 AND
// 0.7, agent-shaped workloads): 4.4-7.2x decode on file re-emission and
// scattered-edit tasks across Qwen3.5-4B/9B and gemma-4-26B-A4B (MoE), with NO
// measurable cost on fresh generation -- and it composes: draft-mtp,ngram-simple
// on the 26B-A4B beat both baseline (+39% fresh) and either type alone.
// Metal is excluded pending its own measurement leg (precedent: v1.27.0
// measured every draft-model mechanism a loss there).
func SpecArgs(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, includeMTP bool) []string {
	var args []string
	if includeMTP {
		args = MTPArgs(cfg, hw, modelPath, serverBin)
		if args == nil {
			// No MTP for this model -- a DFlash head may claim the draft slot
			// instead (MEASURED, 4B/b10298: +57% fresh decode; the case ngram
			// can't speed up). includeMTP=false is the shed-the-draft rung, so
			// DFlash is shed with it -- it costs the same kind of VRAM.
			args = DFlashArgs(cfg, hw, modelPath, serverBin)
		}
	}
	if !ngramActive(cfg, serverBin) {
		return args
	}
	if len(args) >= 2 && args[0] == "--spec-type" {
		args[1] += ",ngram-simple"
		return args
	}
	return append(args, "--spec-type", "ngram-simple")
}

// ngramActive reports whether the ngram-simple drafter should engage: config
// permits, the backend isn't Metal (unmeasured there), and the engine knows the
// type ("" serverBin skips the probe).
func ngramActive(cfg *config.Config, serverBin string) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.Performance.Ngram), "off") {
		return false
	}
	if CurrentBackend() == "metal" {
		return false
	}
	return serverBin == "" || serverSupportsNgram(serverBin)
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
	return append(args, draftCacheArgs(cfg, hw, modelPath)...)
}

// draftCacheArgs quantizes a drafter's OWN KV cache like the main cache. The
// draft context (MTP or DFlash) keeps its own KV (f16 by default) that scales
// with the full window -- at large windows it allocates last and OOMs the
// fullest card (measured: a 768 MiB f16 draft cache at 131K killed the load on
// a 16+12 GB pair). Drafts are verified by the main model, so a lighter draft
// cache only nudges acceptance, never output quality.
func draftCacheArgs(cfg *config.Config, hw platform.Hardware, modelPath string) []string {
	if !cfg.Performance.FlashAttn {
		return nil
	}
	ct := EffectiveCacheType(cfg, hw, modelPath, FileMB(modelPath), WillOffloadExperts(cfg, hw, modelPath))
	if ct == "" || ct == "f16" {
		return nil
	}
	k, v := SplitKV(ct)
	return []string{"--spec-draft-type-k", k, "--spec-draft-type-v", v}
}

// dflashDraftMax is --spec-draft-n-max for DFlash. MEASURED (b10298, 5070 Ti,
// z-lab 4B head Q8, n-max 6): +57% fresh-generation decode, 2.4x re-emission;
// 6 is also the head publishers' recommended block size.
const dflashDraftMax = 6

// DFlashArgs returns the DFlash speculative-decoding flags, or nil when no
// DFlash head sits next to the model, config disables it, MTP already engages
// (both mechanisms need --spec-draft-model -- MTP keeps priority), the backend
// is Metal (unmeasured there, same gate as MTP/ngram), or the engine doesn't
// know the type. Heads are small per-model GGUFs downloaded by the catalog
// flow and paired by filename, exactly like Gemma's external MTP heads.
func DFlashArgs(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string) []string {
	if !dflashActive(cfg, modelPath) {
		return nil
	}
	if serverBin != "" && !serverSupportsDFlash(serverBin) {
		return nil
	}
	head := dflashHeadFor(modelPath)
	if head == "" {
		return nil
	}
	args := []string{"--spec-type", "draft-dflash", "--spec-draft-n-max", strconv.Itoa(dflashDraftMax), "--spec-draft-model", head}
	return append(args, draftCacheArgs(cfg, hw, modelPath)...)
}

// dflashActive reports whether DFlash will engage for this model: a head file
// is present, config permits, MTP does not already claim the draft slot, and
// the backend isn't Metal.
func dflashActive(cfg *config.Config, modelPath string) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.Performance.Dflash), "off") {
		return false
	}
	if visionActive(cfg, modelPath) {
		return false // draft batches break on image embeddings (see mtpActive)
	}
	if mtpActive(cfg, modelPath) {
		return false
	}
	if dflashHeadFor(modelPath) == "" {
		return false
	}
	return CurrentBackend() != "metal"
}

// dflashHeadFor finds an external DFlash drafter head next to a model: a
// "<Family>-DFlash.gguf" file pairs when the model's filename starts with the
// head's family prefix (same rule as MTP heads -- one head serves every quant
// of its model). Returns "" when no head matches.
func dflashHeadFor(modelPath string) string {
	base := filepath.Base(modelPath)
	dir := filepath.Dir(modelPath)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || n == base || !strings.HasSuffix(strings.ToLower(n), "-dflash.gguf") {
			continue
		}
		family := n[:len(n)-len("-DFlash.gguf")]
		if family != "" && strings.HasPrefix(strings.ToLower(base), strings.ToLower(family)) {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// dflashReserveMB is the DFlash allowance for a launch: the head's own weights
// plus its draft context, nonzero only when DFlash will actually engage.
func dflashReserveMB(cfg *config.Config, modelPath string) int {
	if !dflashActive(cfg, modelPath) {
		return 0
	}
	if head := dflashHeadFor(modelPath); head != "" {
		return FileMB(head) + 1024
	}
	return 0
}

// specReserveMB is the total extra-resident VRAM allowance for sizing beyond
// the model weights and KV: whichever drafter will engage (MTP or DFlash)
// contributes its reserve, and so does the vision projector when it will load.
func specReserveMB(cfg *config.Config, modelPath string) int {
	return mtpReserveMB(cfg, modelPath) + dflashReserveMB(cfg, modelPath) + visionReserveMB(cfg, modelPath)
}

// VisionArgs returns the multimodal flags: --mmproj with the projector GGUF
// sitting next to the model (paired by the same family-prefix rule as MTP and
// DFlash heads), or nil when there is no projector or vision = "off". Without
// the projector the language model alone answers image requests with a 500
// ("image input is not supported ... you may need to provide the mmproj") --
// the model card's vision is real, the file just wasn't loaded.
func VisionArgs(cfg *config.Config, modelPath string) []string {
	if !visionActive(cfg, modelPath) {
		return nil
	}
	return []string{"--mmproj", mmprojFor(modelPath)}
}

func visionActive(cfg *config.Config, modelPath string) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.Performance.Vision), "off") {
		return false
	}
	return mmprojFor(modelPath) != ""
}

// mmprojFor finds a vision projector next to a model: a "<Family>-mmproj.gguf"
// file pairs when the model's filename starts with the head's family prefix
// (one projector serves every quant AND the MTP variant of its model).
func mmprojFor(modelPath string) string {
	base := filepath.Base(modelPath)
	dir := filepath.Dir(modelPath)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || n == base || !strings.HasSuffix(strings.ToLower(n), "-mmproj.gguf") {
			continue
		}
		family := n[:len(n)-len("-mmproj.gguf")]
		if family != "" && strings.HasPrefix(strings.ToLower(base), strings.ToLower(family)) {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// visionReserveMB is the projector's VRAM allowance: its weights plus encode
// compute buffers, nonzero only when vision will actually load.
func visionReserveMB(cfg *config.Config, modelPath string) int {
	if !visionActive(cfg, modelPath) {
		return 0
	}
	if p := mmprojFor(modelPath); p != "" {
		return FileMB(p) + 512
	}
	return 0
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
	footprint := float64(modelMB + kvMB + specReserveMB(cfg, modelPath))
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
	ctxCeil  = 262144 // the aim for natively-256K+ models; models trained shorter (lfm2.5-8b-a1b: 128K) are clamped to their own ceiling by clampToTrainedContext, and the load ladder protects the rest

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
	// Draft speculation cannot ride with a loaded vision projector: the draft
	// model can't process image-embedding batches, and llama-server 500s with
	// "failed to process speculative batch" on EVERY image request (MEASURED,
	// b10298: no-spec + vision OK, ngram + vision OK, draft-dflash + vision
	// 500). A vision-active server therefore drops the draft mechanisms and
	// keeps model-free ngram speculation, which survives images.
	if visionActive(cfg, modelPath) {
		return false
	}
	// draft-mtp is off on the Metal backend: originally because it crashed during
	// inference (agent retries forever), and MEASURED (M4 Pro 9B, v1.27.0) it is
	// also a speed LOSS there even when stable -- -8% decode at n-max 1, -15% at
	// the default 2, -38% at 3 vs MTP off. Metal doesn't get the batch-verification
	// parallelism speculation needs. CUDA/Vulkan/CPU keep MTP (its design target).
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
	return hw.VRAMMB-modelMB-gpuReserveMB(hw, modelMB)-specReserveMB(cfg, modelPath) >= minKVHeadroomMB
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
	if IsMoE(modelPath) {
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
	haveMB := hw.VRAMMB - modelMB - gpuReserveMB(hw, modelMB) - specReserveMB(cfg, modelPath)
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
// SYMMETRIC "q4_0" when the q8-sized window would be starved (< StarvedCtxTokens):
// 2x the window at a measured ~+0.3% perplexity (4B, wikitext, b10298 -- within
// the estimate's error bars; upstream's Hadamard KV rotation, merged Apr 2026,
// is what retired the old "4-bit keys cost ~+10% PPL" basis for an asymmetric
// q8_0/q4_0 pair). The asymmetric pair is BANNED as an auto choice: upstream
// prebuilt CUDA binaries ship without FA kernels for MIXED K/V quant types
// (GGML_CUDA_FA_ALL_QUANTS off), so q8_0/q4_0 silently falls back to CPU
// attention -- MEASURED ~19x slower prompt processing (442 vs 8550 tok/s, 4B,
// 5070 Ti, b9672 AND b10298) and -15% decode, and at 442 PP it still passes the
// ppHealthyFloor gate, so nothing downstream catches it. Matched pairs have
// kernels on every backend. Explicit values pass through untouched, including
// an explicit "k/v" pair (the changelog documents the trap).
// Quantized KV needs flash attention; without it the cache is f16 regardless.
func EffectiveCacheType(cfg *config.Config, hw platform.Hardware, modelPath string, modelFileMB int, expertsOffloaded bool) string {
	ct := strings.ToLower(strings.TrimSpace(cfg.Performance.CacheType))
	if ct != "" && ct != "auto" {
		return ct
	}
	if !cfg.Performance.FlashAttn || modelFileMB <= 0 {
		return "q8_0" // no flash-attn (cache is f16 anyway) or unknown size -> never downshift
	}
	// Prefer f16 KV when it costs NO window -- i.e. the f16 cache still reaches
	// the context ceiling, so the window is ceiling-capped either way and f16
	// only buys speed. MEASURED (M4 Pro, 4B, v1.27.0): f16 decodes ~9% faster at
	// an 8k-deep context and processes prompts ~11% faster than q8_0, for 2x the
	// KV bytes -- free when the window is already capped, a capacity loss when it
	// isn't. So f16 only when it reaches the ceiling (free VRAM >= ~8 GB of KV);
	// below that q8_0 keeps the window, and q8_0/q4_0 rescues a starved card.
	// MoE with offloaded experts keeps q8_0: its VRAM math is the expert-offload
	// budget, not the dense case this was measured on.
	if !expertsOffloaded && rawCtxTokens(cfg, hw, "f16", modelPath, modelFileMB) >= ctxCeil {
		return "f16"
	}
	// Starvation is judged on the RAW full-GPU estimate, not the bottom-bumped
	// target: the bump reports a window the KV budget can't actually hold, which
	// would hide starvation from the exact cards the downshift exists for.
	if !expertsOffloaded && rawCtxTokens(cfg, hw, "q8_0", modelPath, modelFileMB) < StarvedCtxTokens {
		return "q4_0"
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
// resolution needs to size the q8 window without recursing). Whatever the sizing
// policy asks for, the answer is capped at what the model was actually trained
// for -- see clampToTrainedContext.
func resolveContextFor(cfg *config.Config, hw platform.Hardware, cacheType, modelPath string, modelFileMB int, expertsOffloaded bool) int {
	want := unclampedContextFor(cfg, hw, cacheType, modelPath, modelFileMB, expertsOffloaded)
	return clampToTrainedContext(modelPath, want, ContextPinned(cfg))
}

// ctxClampWarned tracks models already reported as having a clamped context pin.
var ctxClampWarned sync.Map

// clampToTrainedContext caps a requested window at the model's trained context.
// Above that, llama-server caps the slot itself ("the slot context (N) exceeds
// the training context of the model (M) - capping"), so an unclamped target made
// winc size, PRINT, and estimate decode for a window that never loads -- the
// honesty bug this exists to close. Unknown trained length (0) changes nothing.
// An explicit winc.toml pin is still honoured exactly up to this ceiling; beyond
// it the engine overrules everyone, so winc says so once rather than print a
// number the user will never get.
func clampToTrainedContext(modelPath string, want int, pinned bool) int {
	trained := TrainedContext(modelPath)
	if trained <= 0 || want <= trained {
		return want
	}
	if pinned {
		if _, seen := ctxClampWarned.LoadOrStore(modelPath, true); !seen {
			ui.Warn("context %d exceeds %s's trained context (%d) - using %d; the engine caps it there regardless",
				want, filepath.Base(modelPath), trained, trained)
		}
	}
	return trained
}

func unclampedContextFor(cfg *config.Config, hw platform.Hardware, cacheType, modelPath string, modelFileMB int, expertsOffloaded bool) int {
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
	free := hw.VRAMMB - modelFileMB - gpuReserveMB(hw, modelFileMB) - specReserveMB(cfg, modelPath)
	if free <= 0 {
		return 0
	}
	// Bytes/token depends on the KV cache type, so a smaller cache (q4) fits a
	// proportionally larger window. Default q8_0 keeps the original factor (64).
	toks := free * kvCtxFactor(cacheType, cfg.Performance.FlashAttn)
	return (toks / 8192) * 8192
}

// ContextLadder returns descending context sizes to try (largest fitting first),
// always bottoming out at a workable floor. The target itself is always a rung,
// even under the 16384 agent floor: a sub-floor target only ever comes from an
// explicit winc.toml pin (auto sizing never resolves below ctxFloor), and a pin
// means the user chose the window -- e.g. a small-footprint labeling endpoint,
// not an agent.
func ContextLadder(target int) []int {
	steps := []int{target, 196608, 131072, 98304, 65536, 49152, 32768, 24576, 16384}
	var out []int
	seen := map[int]bool{}
	for _, s := range steps {
		if s <= target && (s >= 16384 || s == target) && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	if len(out) == 0 {
		out = []int{16384}
	}
	return out
}

// ContextPinned reports an explicit numeric context in winc.toml. A pin means
// the user chose the window: the sizing rescues (the starved-KV window climb,
// the bottom-target push) must not raise it. Stepping DOWN when the pinned
// size fails to load remains allowed -- that is load rescue, not override.
func ContextPinned(cfg *config.Config) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Performance.Context)) {
	case "", "auto", "optimal":
		return false
	}
	return true
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
	args = append(args, VisionArgs(cfg, modelPath)...) // image input works only with the mmproj loaded

	modelMB := FileMB(modelPath)
	expertsOff := WillOffloadExperts(cfg, hw, modelPath)
	ngl := GpuLayers(cfg, hw)
	if ctx <= 0 {
		ctx = ResolveContext(cfg, hw, modelPath, modelMB, expertsOff)
	} else {
		// A caller-supplied window (team workers) skips ResolveContext, so clamp
		// here too -- every launch leaves this function with a window the model
		// can actually hold, whoever chose it.
		ctx = clampToTrainedContext(modelPath, ctx, ContextPinned(cfg))
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
		// Pack the leftover: --cpu-moe strands whatever VRAM the window doesn't
		// use (measured: 12 GB idle on a 16 GB card under the 35B-A3B). Keep as
		// many layers' experts ON the GPU as fit after the window is budgeted.
		// MEASURED (35B-A3B UD-Q4_K_M, 5070 Ti, b10298, ctx 32768): decode 47-49
		// -> 81-83 tok/s (+70%), cold pp2048 232 -> 959 tok/s, VRAM 4.3 -> 14.5 GB.
		if n, _, ok := MoEPackPlan(cfg, hw, modelPath, modelMB, ctx); ok {
			args = append(args, "-ngl", "99", "--n-cpu-moe", strconv.Itoa(n))
		} else {
			args = append(args, "-ngl", "99", "--cpu-moe")
		}
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
			// Speculative decoding requires the draft to share the target's
			// vocabulary; a mismatched pair fails inside the engine with an error
			// that points at neither file. When both tokenizers are readable and
			// differ, drop the flag and say so -- the model still serves, just
			// without drafting. Unreadable metadata passes through untouched.
			if DraftMismatch(modelPath, dp) {
				// Once per pair: the launch ladder calls ServerArgs on every
				// context attempt, and one warning is information, five is noise.
				if _, seen := draftWarned.LoadOrStore(modelPath+"|"+dp, true); !seen {
					ui.Warn("draft_model %s has a different tokenizer than %s - speculative decoding off (a draft must share the target's vocabulary)",
						filepath.Base(dp), filepath.Base(modelPath))
				}
			} else {
				args = append(args, "--spec-draft-model", dp)
			}
		}
	}

	// CPU-only inference: pin threads to the PERFORMANCE cores when the OS
	// exposes a P/E split. llama-server's default spans efficiency cores too,
	// and on big.LITTLE parts the E cores gate every token (each layer waits
	// for the slowest worker). Unknown split (0) -> engine default, no guess.
	// GPU backends skip this: prompt+decode run on the GPU there.
	if CurrentBackend() == "cpu" {
		if p := platform.PerformanceCores(); p > 0 {
			args = append(args, "--threads", strconv.Itoa(p))
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
