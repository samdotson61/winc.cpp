# Changelog

All notable changes to winc.cpp, newest first. Each release is a single
`vX.Y.Z: description` commit; tagged releases ship binaries via CI.

## v1.10.0 — 2026-06-10

Workers use the head's leftover VRAM.

### Changed
- Team workers are no longer hard-pinned to the CPU: once the head model is
  resident (it loads first and takes everything it wants), winc re-probes the
  cards and hands the measured leftover VRAM to the workers, largest first --
  the 4B collator is the ladder's information-agent catch-all, so GPU decode
  there speeds up exactly the read/search/fetch subagents that v1.8.0 pinned
  to it. Each claim budgets weights + KV + a compute buffer, keeps a per-GPU
  safety margin, and falls back to a CPU relaunch if the GPU load fails.
  Workers that don't fit the leftover run on the CPU exactly as before; the
  head's VRAM precedence is absolute.

## v1.9.0 — 2026-06-10

Head-first GPU placement.

### Changed
- A head model that fully fits combined VRAM (model + per-GPU buffers + MTP
  draft context + a KV floor) is now forced fully onto the GPU (`-ngl 99`).
  The engine's own device fit is conservative and could spill a layer to the
  CPU on a tight-but-sufficient fit -- on a MoE even one CPU-resident layer
  drags every token through a slow CPU expert pass, competing with the team's
  CPU workers for the cores. The context ladder still steps down and retries
  if a forced load doesn't actually fit. Partial-fit models keep the engine's
  auto placement; explicit `gpu_layers` and Apple unified memory are unchanged.
- Auto-context sizing now budgets the MTP draft context (~1 GB when MTP will
  engage), so a maximum-context ask can't overcommit VRAM and push model
  layers off the GPU.
- `winc detect` plans against the downloaded model file (and its MTP head),
  not just the catalogue estimate.

## v1.8.0 — 2026-06-10

Information-only subagents never escalate to the head model.

### Changed
- Team mode: a subagent request whose every tool is read/search/fetch (an
  explorer, researcher, or fetcher -- or one carrying no tools at all) now tops
  out at the largest CPU worker instead of escalating to the main GPU model,
  regardless of how large its context grows. Opening a second full session on
  the head model just to read and report is strictly slower than a worker and
  competes with the orchestrator for its slots; requests that can act (edit,
  run commands, unknown/MCP tools) keep their right to escalate. This became
  visible after v1.6.0: the combined multi-GPU budget unlocked head escalation
  on machines where it had been capped.
- The end-of-session router stats report these holds as `info-pinned=N`.

## v1.7.0 — 2026-06-10

Gemma 4 MTP.

### Added
- Multi-token prediction for Gemma 4: Gemma ships its MTP heads as a separate
  small GGUF (0.1-0.5 GB) rather than baked into the model. `winc -d` now offers
  the head with every Gemma 4 model, and launch auto-pairs a downloaded head
  (`--spec-type draft-mtp` + `--spec-draft-model`). Measured +11% decode on the
  26B-A4B (draft acceptance 0.80). Needs an engine from 2026-06-07 or later --
  probed, older engines simply run without it.
- The default `mtp_draft_max = 2` was validated against the vendor-suggested 4
  on consumer hardware: 4 drops acceptance to 0.68 and is net slower, so the
  default stands for both Qwen and Gemma.

## v1.6.0 — 2026-06-10

Multi-GPU support.

### Added
- Every GPU is detected (previously only the first nvidia-smi line was read), and
  the memory budget is the combined VRAM of all cards. A 16 GB + 12 GB pair is
  sized as 28 GB: it reaches the large tier, gets larger model recommendations
  and auto-context, and skips MoE expert-offload when the pair fits the model
  that a single card couldn't (verified on real hardware: a 21 GB MoE that
  previously ran with experts in RAM now loads fully across both GPUs at 131K
  context).
- `winc detect` and `winc doctor` list each GPU with its total and free VRAM.

### Changed
- Sizing math reserves a compute buffer per GPU (not one shared) so multi-GPU
  context sizing stays honest.
- Per-card layer placement is deliberately left to the engine's device-memory
  fit, which weighs each card's free VRAM at load time. An explicit
  `--tensor-split` disables that fit and can overpack a card, so winc never
  passes one.

## v1.5.1 — 2026-06-10

Catalog refresh.

### Added
- MTP variants for the dense coders: `qwen3.6-27b-mtp` / `qwen3.6-27b-q5-mtp`
  (built-in multi-token-prediction heads, ~1.4-2.2x decode) and `qwen3.5-9b-mtp`
  for the small tier. The standard entries now point at their MTP builds, so
  `winc -d` surfaces them automatically.
- `gemma4-12b` (Jun 2026) — strongest small-tier generalist: LiveCodeBench 72,
  MMLU-Pro 77.2, multimodal, native tool use, 256K context.

Existing installs pick these up with `winc update` (no reinstall needed).

## v1.5.0 — 2026-06-09

Observability, integrity, and release hygiene.

### Added
- `winc doctor` — read-only health snapshot: hardware, engine binaries
  (file checks only, nothing executed), model files with a GGUF header check,
  config summary (token never shown), agents on PATH, port status, log
  inventory. Doctor only looks; it never starts, stops, or identifies
  processes.
- `winc logs [name] [--bundle]` — print log tails; `--bundle` zips a support
  archive (doctor report + token-redacted winc.toml + all logs) ready to
  attach to a bug report.
- Team-mode worker watchdog: winc notices a dead or unresponsive worker
  (process exit, or 3 failed health checks) and reroutes its traffic up the
  escalation ladder; pinned routes fall back to the main model. Detection
  only — winc never kills or restarts anything.
- Router session stats, printed after the agent exits: requests per backend,
  context-overflow rewrites, max_tokens caps applied, dead-worker reroutes.
- Single-model sessions report how often the context-overflow rewrite saved
  the session.
- sha256 verification of engine downloads against GitHub's published release
  digests. A mismatch is a hard fail (the archive is discarded, never
  extracted); a missing digest (offline tag fallback) proceeds with a note.
- GGUF header validation after every model download — an auth/error page
  saved as a `.gguf` is caught and removed at download time instead of
  failing confusingly at engine load. Pre-existing files are never touched.
- Version stamping: `make release` stamps the binary with the git tag via
  `-ldflags -X`.

### Changed
- winc.toml is now written owner-only (0o600) — it can hold a HuggingFace
  token.
- A download whose connection drops exactly at a chunk boundary is now
  detected by length, kept as `.part`, and resumed on the next run instead
  of being installed truncated.
- CI: gofmt + vet + tests on Linux, Windows, and macOS now gate every
  release build.

### Fixed
- Engine child processes can no longer outlive winc after a hard kill
  (closed console window, Task Manager): Job Objects with
  KILL_ON_JOB_CLOSE on Windows, PDEATHSIG on Linux. Best-effort — normal
  shutdown behavior is unchanged.

## v1.4.x — 2026-06-09
- v1.4.5: halve worker fan-out on <=16GB-RAM systems for double per-agent context
- v1.4.4: cap worker generation (loop guard) so a runaway can't burn minutes of CPU
- v1.4.3: always front single mode with the router so the overflow rewrite applies in every reasoning mode
- v1.4.2: rewrite llama context-overflow into Claude Code's "prompt is too long"
- v1.4.1: family-correct sampling for all tiers (not just small/nano)
- v1.4.0: compact worker requests (per-tier tool allowlist + minify + cache-reuse)

## v1.3.x — 2026-06-09
- v1.3.6: RAM-fit team workers smallest-first instead of all-or-nothing
- v1.3.5: team auto-engages for any model above the nano tier (with RAM for the workers)
- v1.3.4: team auto-engages for any >=8GB model with RAM for the workers
- v1.3.3: raise local-model timeouts so slow / low-end boxes don't error mid-turn
- v1.3.2: winc update reconciles winc.toml (repair stale default_model, add new sections)
- v1.3.1: add an optional 2B middle rung to the dynamic ladder
- v1.3.0: dynamic infra-driven subagent tiering + pre-approved web search

## v1.2.x — 2026-06-08
- v1.2.2: team mode by default + force subagents (incl. Workflow fan-out) onto the small worker
- v1.2.1: reliable tool use for nano models (low-think + loop-safe sampling)
- v1.2.0: agent team mode (--team) — big model orchestrates small CPU workers

## v1.1.x — 2026-06-06 to 2026-06-08
- v1.1.20: fix 400 "Unable to generate parser" on Qwen3.5 templates
- v1.1.19: prune to a 2026-only catalog (Qwen3.5/3.6 + Gemma 4)
- v1.1.18: Apple Silicon memory haircut so Macs aren't over-recommended
- v1.1.17: winc update always rebuilds (clone) + confirms/skips engine refresh
- v1.1.16: stop proxy "context canceled" noise from bleeding into the agent's terminal
- v1.1.15: skip thinking on context-compaction requests
- v1.1.14: halve the auto-compaction buffer (85% -> 93%)
- v1.1.13: macOS fixes (Terminal glitches, MTP infinite-retry), accurate MTP sizes + cache freshness
- v1.1.12: larger context for tight-fit MoE/MTP + tell Claude Code the real window
- v1.1.11: winc update pulls ALL repo files + rebuilds (clone); check reports staleness
- v1.1.10: winc update refreshes the model catalog too
- v1.1.9: MTP (Multi-Token Prediction) support for Qwen3.6 variants
- v1.1.8: auto-paired speculative drafts for dense models + cache-type-aware context + accurate sizes
- v1.1.7: MoE expert offload + speculative decoding + Performance docs
- v1.1.6: recommend MoE models for mid/large tiers (speed-first)
- v1.1.5: detect dedicated VRAM for AMD and Intel GPUs
- v1.1.4: size model recommendations by dedicated VRAM, not shared/system RAM
- v1.1.3: wait for the model to load (/health) before launching the agent
- v1.1.2: fix Linux/macOS engine shared-library loading
- v1.1.1: robust engine backend selection for low-end / older-driver GPUs
- v1.1.0: cross-platform install + vendored offline build; context fix

## v1.0.0 — 2026-06-06
- Full conversion to a single portable Go binary (no PowerShell, no Python);
  README + MIT license.

## v0.x — 2026-06-05 to 2026-06-06
- Original script-based prototype: installer, model management, launcher,
  truecolor/tmux fixes, uninstall. Superseded by the Go rewrite in v1.0.0.
