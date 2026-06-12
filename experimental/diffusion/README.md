# DiffusionGemma experiment (branch: experimental)

Status: UNCOMMITTED EXPERIMENT — nothing here ships without explicit go-ahead.

## What this is

Viability test of DiffusionGemma 26B-A4B (Google's block-diffusion Gemma 4 MoE,
256-token denoising canvas) as a faster alternative for low-end and mid-tier
devices to clear the 40-50 tok/s decode bar.

The engine support is llama.cpp DRAFT PR #24423 (`llama-diffusion-cli` only —
no llama-server support, no OpenAI-compatible endpoint). winc cannot front this
for Claude Code or jobdar yet; this experiment measures whether the speed
story justifies tracking the PR toward a future integration.

## Layout

- `build.ps1` — clone + checkout PR 24423 + CUDA build of llama-diffusion-cli
  (engine clone lives at C:\Claude\llamacpp-diffusion, OUTSIDE this repo)
- `bench.ps1` — the VRAM-tier matrix: CPU floor, ~2GB / ~4GB proxies (-ngl
  steps), 12GB (3060 solo), 16GB (5070 Ti solo), 28GB (both cards), each
  measured for real VRAM use and decode tok/s; autoregressive
  gemma-4-26B-A4B at matched tiers as the baseline
- `results.md` — measured output (generated)

## Model files

- Diffusion: models\diffusiongemma-26B-A4B-it-Q4_K_M.gguf (15.65 GB)
- AR baseline: models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf (12.68 GB, already
  present; quant not identical to Q4_K_M — tier-class comparisons only)
