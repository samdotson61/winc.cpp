# Changelog

All notable changes to winc.cpp, newest first. Each release is a single
`vX.Y.Z: description` commit; tagged releases ship binaries via CI.

## v1.15.0 — 2026-06-11

The agent now knows its real window -- and what it forgets is recoverable.
Driven by a forensic post-mortem of an overnight session that died blind: the
agent believed a 100k window on a real 32k slot, filled it in 26 minutes,
burned 20+ turns on tool calls whose arguments were silently truncated at the
wall (no error -- llama-server just stops generating), and the recovery
compaction summarized the garbage tail of the transcript into 154 tokens.
Total amnesia by morning.

### Fixed
- Window truth: Claude Code 2.1.x validates CLAUDE_CODE_AUTO_COMPACT_WINDOW
  against a 100,000-token MINIMUM (verified against the 2.1.173 binary) -- the
  real window winc passed was rejected as invalid and the agent silently
  believed the 100k default, so its preemptive compaction could never fire
  before the real wall. winc now reports a window Claude Code will accept and
  places the compaction trigger ABSOLUTELY via the percentage override (an
  unclamped parseFloat): pct of the believed window == the real window minus
  the max(8k, window/8) reserve. Windows >= 100k keep the original behavior
  exactly.
- Team escalation no longer creates unworkable heads: --parallel 2 halves the
  per-slot window, and Claude Code's fixed overhead (system prompt + tools) is
  ~24k tokens on its own -- a 32k slot starves. Escalation now engages only
  when the expected half (launch memo, else sizing target) stays >= 48k, and a
  post-launch guard relaunches unsplit if the ladder lands lower. Single/serve
  mode was verified unaffected: without --parallel the engine runs a unified
  KV pool and every request can use the full window.
- Router preflight: a head-bound request whose estimated size leaves less than
  2k tokens of generation room is answered with the exact "prompt is too long"
  signal Claude Code recognizes, BEFORE the server can accept it and truncate
  generation silently (status 200, truncated=1, tool-call arguments lost).
  Counts into the session's overflow stats. Defense in depth against any
  future upstream window-handling drift.

### Added (memory layer, §0 M1+M2 of the spec)
- Compaction-trim archive: the oldest messages the router drops from an
  oversized compaction request are flattened into
  .claude-local/trimmed-context.md (timestamped, grep-friendly, 5 MB rotation)
  BEFORE they vanish -- they are exactly what the summary won't cover, and
  this session proved the summary can be 154 tokens of nothing. Best-effort:
  archive failures never touch the request path.
- Agent scaffolding: every launch writes .claude-local/CLAUDE.md (the
  sandboxed session's user-memory file) with the REAL window, the per-slot
  share, the launch's measured decode/prompt speeds, and small-window working
  practices -- persist state to a notes file early, grep the trim archive
  after compaction, check the task list before asking the user what to do.
  Rewritten only while the winc marker leads the file; user edits own it
  forever after.

## v1.14.4 — 2026-06-11

### Fixed
- CI: the new placement-gate test sized its model fixtures as multi-GB
  truncated files. POSIX filesystems keep those sparse; NTFS allocates them for
  real, which exhausted the Windows runner's disk and blocked the v1.14.3
  release build (binaries for v1.14.3 ship with this tag). The test now feeds
  sizes to the sizing logic directly -- no fixture files at all.

## v1.14.3 — 2026-06-11

A model that should be fully in VRAM now provably IS -- or the launcher steps
down until it is.

### Fixed
- Silent shared-memory fallback. When a pinned full-GPU load (-ngl 99) exceeds
  free dedicated VRAM, the Windows driver can satisfy the allocations from
  SHARED system memory instead of failing: the server passes /health, answers
  small completions normally -- and the first real prompt crawls (observed
  live: a 19 GB model "loaded" with both cards still ~93% free, ~20 GB
  committed in system RAM, and the agent's first message processing at 50-125
  tok/s, decelerating, where ~30 seconds was normal). The load looked
  measured-good, so the launch memo then replayed the broken placement on
  every start. Every forced-full-GPU, auto-sized load is now verified after
  /health by a placement gate:
  - residency: free dedicated VRAM must drop by at least HALF the model's size
    across the load (a resident model consumes at least its own weights);
  - throughput: a ~2.5k-token bench prompt must clear 150 tok/s of batched
    prompt processing (GPU-resident measures many hundreds; sysmem-paged
    weights measured 50-125).
  A rung that fails either is treated exactly like a failed load -- the ladder
  steps down and the memos record only gated-healthy rungs (existing poisoned
  memo entries self-heal on the next launch). The floor rung and last-resort
  reloads always accept, warning loudly, so the gate can never block a launch.
  Explicit gpu_layers/context settings run as written, ungated.
- Team mode could finish the job: the leftover-VRAM probe read the phantom
  "free" cards as ~24 GB of leftover and seated workers on top of the paged-out
  head. Two guards now: leftover is sanity-checked against what a resident head
  could possibly leave (impossible numbers keep every worker on the CPU), and
  when any worker does claim VRAM the head's prompt speed is re-measured
  afterwards -- if it degraded, those workers are stopped and relaunched on the
  CPU (the head's residency outranks worker speedup, always).

### Changed
- The remembered launch (.winc-launch) is re-verified by the gate on every
  start -- free VRAM drifts day to day, and a window that was healthy yesterday
  can be over the cliff today. The launch speed report now includes measured
  prompt processing alongside decode (e.g. "decode: ~36 tok/s (prompt ~620
  tok/s)"), from the gate's own bench -- a launch is never benched twice.

## v1.14.2 — 2026-06-10

Faster launches and a leaner router. Pure overhead removal -- every decision,
rewrite, and report behaves exactly as before.

### Changed
- The request router parses each chat request ONCE and runs the whole rewrite
  pipeline (compaction trim, tool strip + minify, thinking policy, max_tokens
  clamp) against that single parsed form, re-encoding once at the end. The old
  pipeline re-decoded and re-encoded the full body at every stage; on a
  late-session transcript (~1 MB) that was ~33 ms and ~11 MB of allocation per
  request, now ~15 ms and ~5.7 MB (measured). Untouched requests still pass
  through byte-identical, so prefix-cache reuse is unaffected.
- Request scans (code fences, tool markers) read the raw bytes instead of
  making lowercased copies of the whole body -- two full-body copies per
  request gone.
- The launch decode bench is remembered in the launch memo (.winc-launch) next
  to the measured window + KV cache: a warm launch reports the remembered
  speed instead of re-running the bench completion (seconds saved per start).
  Anything that re-measures the memo re-measures the speed; deleting the file
  forces both.
- VRAM polls (the post-stop drain wait, team mode's leftover-VRAM probe) re-read
  only the per-GPU memory snapshot -- one nvidia-smi call -- instead of a full
  hardware re-detection per poll, and the driver's CUDA version is probed once
  per process instead of on every detection.
- Server readiness is polled every 250 ms instead of every 1 s, so each context
  rung and each team worker start stops wasting most of a second.
- `winc check` runs its three release lookups, the local engine version probe,
  and the git status fetch concurrently -- the check waits for the slowest one
  instead of all five in sequence. `winc update` reuses one GitHub release
  answer per repo instead of re-fetching it for the version check, the asset
  list, and the digest verification.

## v1.14.1 — 2026-06-10

Fast, accurate VRAM feasibility -- no more multi-minute failed loads.

### Fixed
- A context rung that couldn't fit cost a full weight upload (3+ minutes cold)
  before the allocation failure surfaced -- and the total-VRAM formula couldn't
  see ONE card running out (the smaller GPU's share + its compute buffers + the
  MTP draft context, which allocates last). The ladder and the upgrade probe now
  consult the engine's own placement calculator (llama-fit-params: metadata
  only, seconds, no weight upload) before every attempt, skipping rungs it says
  can't stay fully on GPU. For MTP models -- whose draft context (~2 GB at
  large windows) the calculator can't see -- the verdict must hold 2 rungs
  higher for cold loads (1 for staircase probe rungs); these margins reproduce
  every measured outcome on real 16+12 GB hardware. The floor rung is always
  attempted, so the estimator can never block a launch.
- The model's transformer block count is read from GGUF metadata to define
  "every layer on the GPU" exactly.

## v1.14.0 — 2026-06-10

Quality floor + smarter recommendations.

### Added
- Catalog quality floor, enforced by test: nothing below IQ3-class quantization
  anywhere, nothing below Q4_K_M for models under 14B (destructive quants make
  small models useless, coding especially). gemma4-12b moves Q3_K_M -> Q4_K_M.
- Catalog variants (all verified): qwen3.5-4b-q8 (4.5 GB), qwen3.5-9b-q5
  (6.6 GB), qwen3.6-27b-q6 + MTP (22.5 GB), qwen3.6-35b-q5 + MTP (26.6 GB),
  qwen3-coder-next-iq3 (29.7 GB, for 48-64 GB Macs).
- Conservative recommendations: the recommended model must leave ~2 GB of
  runtime headroom within the memory budget, stepping DOWN a tier when the
  budget tier has nothing honest (a 4 GB card gets the 2B, a 12 GB card gets
  a small-tier model that runs well instead of a 13.6 GB flagship).
- Uncatalogued Qwen3.5-family downloads auto-pair with the 0.8B speculative
  draft (same tokenizer family; MoE/MTP/tiny files excluded) -- unknown models
  now get the full optimization treatment (sampling, MoE/MTP detection,
  template patching, and KV sizing were already filename/GGUF-based).

### Changed
- `context = "optimal"` is the new default sizing mode: ~128K total per agent
  slot (~64K effective working context + system prompt + auto-compaction
  buffer), keeping decode in the 40-80 tok/s band; team escalation doubles the
  total so each slot keeps the full baseline. `context = "auto"` keeps the
  previous behavior (the largest window that fits, up to the 256K ceiling).
- Every launch now MEASURES decode speed with a small completion and reports
  it; below 40 tok/s winc says so and points at faster picks.
- The starved-window KV downshift is now ASYMMETRIC: keys stay q8_0, values
  drop to q4_0 (4-bit keys measure ~+10% perplexity -- past the usefulness
  line; 4-bit values are near-lossless). Applies to the MTP draft cache too.
  cache_type accepts an explicit "k/v" pair.
- Team workers pin their KV cache to q8_0: small models are the most
  sensitive to KV quantization, and worker windows are tiny anyway.

## v1.13.0 — 2026-06-10

Up to 2x the context window, measured not guessed.

### Added
- Context ceiling raised 131K -> 262K (every 2026 catalog model is natively
  256K+), with matching ladder rungs.
- `cache_type = "auto"` (new default): q8_0 normally, q4_0 when the sized
  window would be starved -- roughly doubling it on low-VRAM machines.
- Launch-time KV upgrade probe: when the ladder settles below the sizing
  target (context-scaled overheads the formula can't see), winc probes the
  next rungs with q4_0 KV caches and keeps the widest window that loads.
  Outcomes are memoized per model (.winc-kvprobe) so the probe's failed-load
  cost is paid once, ever. Measured on a 16+12 GB pair: the 35B MoE goes
  131072 -> 262144 fully on GPU at full decode speed (93.6 tok/s).
- The MTP draft context's own KV cache (f16, scales with the full window --
  it OOM'd the smaller card at 131K+) is now quantized to match the main
  cache (--spec-draft-type-k/v). Drafts are verified by the main model, so
  this never affects output quality.
- Launch memo (.winc-launch): the first start of a model measures its best
  window + cache; every later start loads ONCE straight to it instead of
  re-walking the ladder (which is minutes of failed jumbo loads at the new
  ceiling). Validated each start -- a stale entry just re-measures. Only
  applies when sizing is on auto; explicit settings run as written.

### Changed
- Forced full-GPU placement is kept for every fully-fitting model, including
  during the probe: the engine's spill-happy auto-fit measured 2-4x slower
  decode than full-GPU at every context size.
- The starved-window check accounts for --parallel slot splitting (team mode
  halves the per-agent window).

## v1.12.0 — 2026-06-10

Long sessions auto-compact and keep going.

### Fixed
- The compaction death loop: at local window sizes the auto-compaction trigger
  (93%) left only a sliver of headroom -- one big tool result jumped straight
  past the end of the window, and the recovery compaction (whole transcript +
  summary) then hit the context wall mid-summary, shrinking nothing. The
  session looped on the overflow forever (observed live at a 49k window:
  overflow -> truncated summary -> overflow, every ~90s for half an hour).
  Two layers now prevent it:
  - The compaction trigger reserves max(8k, window/8) tokens of real headroom
    for the in-flight turn plus the summary generation.
  - The router trims the OLDEST transcript messages out of a compaction
    request that no longer fits (keeping the summarize instruction and opening
    on a clean user message), so the summary always has room to complete --
    the session compacts and keeps going, like the cloud endpoints do.

## v1.11.0 — 2026-06-10

`winc -s` resumes the last used setup.

### Changed
- Every successful agent start (single and team mode) persists its agent and
  model as the new `default_app` / `default_model`, so a bare `winc -s` brings
  back the last used model with the last used agent. Only an explicitly named,
  successfully resolved model updates the defaults -- a typo never becomes the
  default -- and the `cli` chat utility is excluded so a quick test chat
  doesn't flip them. winc.toml is edited in place (just those two values);
  everything else in it is untouched.

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
