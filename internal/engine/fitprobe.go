package engine

import (
	"os/exec"
	"regexp"
	"strconv"
	"sync"

	"winc/internal/config"
	"winc/internal/paths"
)

// The engine ships llama-fit-params: its own placement calculator, which answers
// "what fits at this context with this KV cache" from model METADATA in a few
// seconds -- no weight upload. winc uses it as a pre-filter so the context ladder
// never pays a multi-minute weight upload just to discover an allocation failure
// at the end (measured: a single failed 131K rung on a cold cache cost 3+ minutes).

// LlamaFitParamsPath returns the fit calculator's path, "" when not installed.
func LlamaFitParamsPath() string { return findIn("llama-fit-params", paths.BinDir()) }

var fitNGL = regexp.MustCompile(`-ngl (-?\d+)`)

// parseFitNGL extracts the fitted -ngl from the calculator's output line.
// -1 means "everything fits trivially".
func parseFitNGL(out string) (int, bool) {
	m := fitNGL.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

var (
	fitVerdictMu    sync.Mutex
	fitVerdictCache = map[string]bool{}
)

// FitVerdictFull reports whether the engine's own fit calculator says EVERY layer
// fits the GPUs at this context with this KV cache. ok=false when the tool or the
// model's block count is unavailable -- the caller then falls back to simply
// attempting the load (the attempt remains the ground truth either way).
func FitVerdictFull(cfg *config.Config, modelPath string, ctx int, cacheType string) (full, ok bool) {
	bin := LlamaFitParamsPath()
	blocks := BlockCount(modelPath)
	if bin == "" || blocks <= 0 {
		return false, false
	}
	key := modelPath + "|" + strconv.Itoa(ctx) + "|" + cacheType
	fitVerdictMu.Lock()
	if v, hit := fitVerdictCache[key]; hit {
		fitVerdictMu.Unlock()
		return v, true
	}
	fitVerdictMu.Unlock()

	args := []string{"-m", modelPath, "-c", strconv.Itoa(ctx), "-b", "2048", "-ub", "512"}
	if cfg.Performance.FlashAttn {
		args = append(args, "--flash-attn", "on")
		if k, v := SplitKV(cacheType); cacheType != "" && cacheType != "f16" {
			args = append(args, "--cache-type-k", k, "--cache-type-v", v)
		}
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return false, false
	}
	ngl, parsed := parseFitNGL(string(out))
	if !parsed {
		return false, false
	}
	// -1 = trivially fits; an explicit count >= block_count = every block placed
	// on a GPU (verified against real models: the 27B reports -ngl 65 with 65
	// blocks when fully resident).
	full = ngl == -1 || ngl >= blocks
	fitVerdictMu.Lock()
	fitVerdictCache[key] = full
	fitVerdictMu.Unlock()
	return full, true
}

// MTPActive reports whether MTP will engage for this model (sizing-level check).
func MTPActive(cfg *config.Config, modelPath string) bool { return mtpActive(cfg, modelPath) }
