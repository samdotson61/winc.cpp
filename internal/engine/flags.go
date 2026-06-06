package engine

import (
	"os"
	"strconv"

	"winc/internal/config"
	"winc/internal/platform"
)

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
	toks := free * 64 // ~16 KB/token for q8_0 KV (conservative)
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
	args = append(args, "-ngl", strconv.Itoa(ngl))

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
	return args
}
