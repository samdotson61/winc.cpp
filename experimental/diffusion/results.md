# DiffusionGemma 26B-A4B viability results (2026-06-12)

Hardware: RTX 5070 Ti 16GB (CUDA0) + RTX 3060 12GB (CUDA1), Ryzen 7 7700X, 32GB DDR5.
Engine: llama.cpp draft PR #24423 @ 7a6ddc54, CUDA 13.3, ninja/MSVC 18 build.
Model: diffusiongemma-26B-A4B-it-Q4_K_M.gguf (15.65 GB). Prompt: jobdar-style JSON
eval (~160 tok). 2 blocks x 256-token canvas, temp 0, seed fixed, -fa on.
AR baseline: gemma-4-26B-A4B-it-UD-IQ4_NL (12.66 GB) via shipped llama-bench,
same tier shapes (tg64; tg64@d16384 in parens).

## Decode, diffusion (effective tok/s) vs autoregressive, by VRAM tier

| Tier shape                     | Diffusion (defaults) | AR baseline      | AR advantage |
|--------------------------------|----------------------|------------------|--------------|
| 28GB, both GPUs, -ngl 99       | 19.1                 | 78.8 (70.5)      | 4.1x         |
| 16GB, 5070Ti solo, -ngl 99     | OOM (16GB cudaMalloc)| 152.5 (128.6)    | --           |
| 16GB, 5070Ti, exps=CPU         | 12.9                 | --               | ~11x vs res. |
| 12GB-class, 3060, half exps    | 3.2                  | 34.8 (32.0)      | 10.9x        |
| 4GB-class, 3060, all exps CPU  | 1.8                  | 25.0 (24.1)      | 13.9x        |
| 2GB-class, -ngl 15 + exps CPU  | 1.6                  | 19.4 (14.2)      | 12.1x        |
| CPU only                       | 12.7                 | 15.5 (10.5)      | 1.2x         |

In-step parallel rates (256-tok canvas / step time) read impressively (25-504
tok/s) but entropy-bound needs ~16-18 steps/block at quality -> effective =
parallel / steps.

## Tuning ladder (28GB tier)

| Config                                  | tok/s | Output quality            |
|-----------------------------------------|-------|---------------------------|
| defaults (kv-cache auto = OFF)          | 19.1  | good JSON                 |
| --diffusion-kv-cache on                 | 28.0  | good JSON                 |
| kv on + --diffusion-eb-max-steps 12     | 39.8  | good JSON (score+reasons) |
| kv on + --diffusion-eb-max-steps 8      | 59.9  | GARBAGE (degenerate loops)|

Low-end best case: 4GB-class + kv on + steps 12 = 2.8 tok/s (7.7 s/step).

## Why low-end inverts

Each denoise step forwards the whole [prompt | canvas]; a 256-position canvas
routes to ~all 128 experts per layer (AR touches 8 per token), so expert-in-RAM
tiers pay near-full expert reads EVERY step (~8-10 s/step on the 3060 hybrid).
CPU-only beats GPU+CPU-expert hybrids (12.7 vs 1.8) -- the hybrid adds PCIe
ping-pong on top. Block-diffusion's weight-read amortization helps exactly
where weights are already fast (full VRAM) and hurts where they are slow.

## Other findings

- Q4_K_M does not load on a 16GB card (single 16013 MiB weight buffer).
- KV-cache "auto" left the optimization OFF in this PR state; ON is strictly
  better in these tests (19.1 -> 28.0 same quality).
- Quality cliff between 12 steps (clean) and 8 steps (degenerate repetition).
- Multi-turn: PR thread reports ~60% step-time growth by turn 5 (prefix grows
  into every step's forward); not re-measured here -- tier verdict already
  decisive without it.
- JSON shape held at 12+ steps on every tier tested, including a bilingual
  candidate field -- the MODEL is jobdar-capable; the runtime/speed is not.
- "<channel|>" template artifacts appear in replies (template handling is
  immature in the PR).

## Verdict

NOT viable as the low/mid-tier speed path. Diffusion loses to the SAME
weights run autoregressively at every GPU tier on consumer hardware, by 4-14x,
and worst exactly at the low end (1.6-3.2 tok/s vs winc's 40-tok/s target).
The AR 26B-A4B at 16GB (152 tok/s) or the FFN-spill/4B path at 4GB remain the
real routes to the target. Becomes worth revisiting only if upstream lands:
true llama-server diffusion support, expert-sparse canvas routing (MoE-aware
denoising), or a distilled 4-6-step variant that keeps quality.
