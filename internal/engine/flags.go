package engine

import (
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

// ServerArgs assembles llama-server arguments from config + hardware for a model.
// portPlaceholder, if non-empty, is used verbatim instead of a numeric port
// (llama-swap needs the literal "${PORT}").
func ServerArgs(cfg *config.Config, hw platform.Hardware, modelPath string, port int, portPlaceholder string) []string {
	portVal := strconv.Itoa(port)
	if portPlaceholder != "" {
		portVal = portPlaceholder
	}
	args := []string{"-m", modelPath, "--host", cfg.General.Host, "--port", portVal, "--jinja"}

	ngl := GpuLayers(cfg, hw)
	args = append(args, "-ngl", strconv.Itoa(ngl))

	// Default large enough for Claude Code's system prompt + tool schemas (~25k
	// tokens) plus conversation headroom. Measured to fit the tightest tier case
	// (35B on 16 GB) with q8_0 KV + flash-attn. Override with context= in winc.toml.
	ctx := 32768
	if cfg.Performance.Context != "auto" && cfg.Performance.Context != "" {
		ctx = atoiOr(cfg.Performance.Context, ctx)
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

	// Reasoning: static modes set server flags; adaptive runs the server in "auto"
	// and lets winc-router cap the budget per request.
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
