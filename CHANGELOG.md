# Changelog

All notable changes to winc.cpp, newest first. Each release is a single
`vX.Y.Z: description` commit; tagged releases ship binaries via CI.

## 1.21.2-jobdar.1 — 2026-06-12 (winc-jobdar branch)

The jobdar evaluation profile. This branch carries jobdar-specific stability
work on top of master releases; it is built from the branch, never tagged as
a master release. First released as 1.21.1-jobdar.1; rebased onto v1.21.2,
which upstreamed this branch's two fixes (template-level reasoning off, the
branch self-update guard).

### Added
- `winc serve --eval [model]` — the jobdar inference profile. Every knob is a
  measured choice for the eval shape (a 1-5k-token posting+resume prompt, a
  few-hundred-token JSON verdict, many independent calls):
  - the ROUTER binds the winc.toml port (default 8080): jobdar configures ONE
    stable Anthropic `/v1/messages` URL (`inference_url`); llama-server moves
    to an ephemeral port behind it. Agent flows keep their ephemeral router.
  - reasoning OFF and speculative draft OFF: thinking returns EMPTY content
    with the whole token budget spent, and the 0.8B draft measured a ~50%
    decode LOSS at this shape on every tier (it is 20-40% of these targets'
    size). No MTP, no team mode.
  - 16384-token window, q8 KV: the whole 2B server fits a 2 GB card (~1.7 GB),
    the 4B a 4 GB card (~3.4 GB).
  - model auto-pick when none is named: >=6 GB-class VRAM prefers the 4B
    (better judgment on requirement mismatches), smaller cards and CPU-only
    prefer the 2B (143 tok/s on a 12 GB-class card, ~42 tok/s on a desktop
    CPU); falls back to whichever is downloaded, else prints the exact
    `winc -d` command.
  - the engine's auto-parallel UNIFIED KV pool serves jobdar's scan-stage
    concurrency as-is (measured: 3 concurrent evals at 118 tok/s aggregate
    where a single stream runs ~98).

## v1.21.2 — 2026-06-12

### Fixed
- `reasoning = off` (and the `--reasoning off` CLI flag) now emits the
  engine's template-level `--reasoning off` instead of `--reasoning-budget 0`.
  Measured on Qwen3.5 (2B and 4B): budget-0 still routes every generated
  token into the thinking channel -- the client receives EMPTY content with
  max_tokens fully spent. Template-level off answers in content at full speed.
- `winc update` refuses the prebuilt self-update on winc-jobdar branch builds
  (versions containing "jobdar"): replacing the binary with a master release
  would silently drop the branch's stability profile. Engine + catalog refresh
  still run; branch users update from the branch.

## v1.21.1 — 2026-06-12

### Fixed
- `winc -v` / `--version` have dispatched to `winc version` since v1.0.0 but
  were never listed in the help text, so the aliases were undiscoverable.
  `winc help` now shows them.

## v1.21.0 — 2026-06-12

Dense models that can't afford their window now spill ONLY feed-forward
weights -- attention and the whole context stay on the GPU.

### Added
- FFN-only spill (dense models): when full residency can't reach the bottom
  context target even with the MTP draft dropped, the launcher now parks the
  FEWEST trailing blocks' feed-forward weights in system RAM (-ngl 99 plus a
  tensor override) instead of jumping straight to whole-layer engine spill.
  Everything that reads the context -- every attention/SSM tensor and the
  entire KV cache -- stays GPU-resident. Measured against whole-layer spill:
  more decode per spilled byte (a 27B with 8 blocks' FFN in RAM decodes 15.5
  tok/s where -ngl 56 manages 10.5 with MORE VRAM still used), and -- the
  property that matters in real agent sessions -- the rate holds FLAT as the
  context fills (FFN-spilled 4B: 28.2 tok/s empty, 27.1 at a 32k-deep
  context; whole-layer: 36.5 empty collapsing to 24.1 by 16k and still
  falling). Feed-forward weights are 50-62% of these models' bytes (measured
  exactly from the GGUF tensor table via offset deltas -- no quant-size
  tables to maintain), so the relief per spilled block is large and the
  spill count comes from the actual KV deficit, not a guess. One bumped
  retry covers estimate misses; the placement gate verifies every attempt
  against the REDUCED resident size; the launch memo records "ffn:n" so
  replays load identically. MoE models never take this path -- expert
  offload (--cpu-moe) is their cheaper version of the same trade.
- Sub-bottom FFN descent (the 4 GB class): when even every FFN block in RAM
  can't afford the ~100k bottom target, the launcher tries 65536/49152/32768
  with the deficit-sized spill before surrendering to engine placement at the
  bottom. A gate-VERIFIED window with resident attention that decodes at a
  flat ~30-40 tok/s beats an unverifiable everything-through-RAM window that
  starts slow and decays with depth -- on the hardware this targets, the
  measured difference at working depths is 2-4x. The decode report and the
  memo state exactly what was traded.
- Live-verified on real hardware (12 GB card, 4B): a 262144-token window
  with the full KV cache resident and all FFN in RAM -- 459 tok/s prompt on
  a 10k-token prompt, 29.2 tok/s decode, both in the band the offline bench
  matrix predicted.

### Notes
- The placement gate's prompt floor (150 tok/s) is unchanged: healthy
  FFN-spilled loads measured 434-804 batched prompt speed across both test
  models; the sick signature it exists to catch stays 50-125.
- The engine suggests --no-mmap when tensor overrides target the CPU;
  measured numbers above are WITH mmap (the default). A future knob may trade
  commit-on-load for steadier CPU reads on RAM-rich boxes.

## v1.20.0 — 2026-06-11

Multi-GPU decode packs the fast card first.

### Added
- Bandwidth-weighted tensor split: on multi-GPU machines, forced-full-GPU
  loads now pass an explicit --tensor-split that packs the FASTEST card to its
  budget instead of balancing the cards. Decode on a layer split is ADDITIVE
  per card (t = sum of bytes_i / bandwidth_i), and the engine's free-ratio
  default -- which is all a pinned -ngl gets, since the pin aborts the
  engine's own device fit -- left measured gigabytes of the fast card idle
  while the slow card gated every token (a 5070Ti+3060 pair measures 460 vs
  210 tok/s solo: 2.19x). Per-card speeds are MEASURED, not assumed: the small
  probe model is loaded entirely on each card once (-sm none -mg N, ~15s per
  card, a near-pure memory-bandwidth probe) and the result is cached in
  .winc-hw forever. No probe model / single GPU / unmeasured cards -> the
  engine default stands. The placement gate verifies every split load; a bad
  split steps down like any failed rung. (This retires the v1.6.0 "never pass
  --tensor-split" rule for pinned loads -- the engine fit it protected is
  already aborted there, and the gate now catches what that rule guarded
  against.)
- Bottom-target stage 0: with a measured split, the launcher first retries the
  bottom window WITH the MTP draft before sacrificing it -- the balanced
  default failed those loads by overflowing ONE card, not by total.

### Fixed
- The KV-upgrade probe attempted the next rung while the current best server
  was STILL RESIDENT, betting on a fast OOM to break the climb. The placement
  gate turned that bet toxic: with both near-full loads resident, the doomed
  attempt "loads" into shared system memory instead of failing, measures sick
  -- and the concurrent pressure can take the GOOD server down with it
  (observed live: the accepted rung died orphaned while the rejected one owned
  its port, and the final report carried the dead attempt's numbers). The
  climb now stops the best server BEFORE each attempt and reloads the best
  rung when an attempt fails -- every rung gets a clean solo verdict, and the
  report/memo numbers always belong to the server that actually serves.
- The first split cut left only ~300 MB of slack on the packed card, and CUDA's
  per-card compute buffers don't shrink with the split -- a no-MTP load that fit
  UNSPLIT was OOM'd by the split itself, costing the resident rung and falling
  through to engine spill (strictly worse). Each card now keeps a margin of
  max(1 GB, its proportional share of the total reserve), and the split is
  never load-bearing: any failed or gate-rejected split load is retried once
  with the engine's default placement before the rung is declared dead, so the
  split can only ever IMPROVE an outcome, not take one away.
- The placement gate's bench probe occasionally came back empty on a healthy
  just-ready server and the rung was "accepted unverified" -- skipping the very
  residency check the gate exists for. An empty first bench (not a slow one)
  now pauses 2s and retries once before giving up; a measured pass prints the
  number it saw.

## v1.19.0 — 2026-06-11

One universal sizing policy, a pooled KV cache, and no more half answers.

### Changed
- POOLED KV: team escalation no longer passes --parallel 2. The engine's auto
  mode runs a UNIFIED KV pool (verified on the shipped engine: every sequence
  may use the full window), so the head keeps its WHOLE context and an
  escalated subagent borrows from the pool only while it runs. The explicit
  flag hard-split the window -- half the head's context gone even with zero
  subagents active. (Across two discrete GPUs, buffers can never pool --
  PCIe-separated memory, each card hosts its own layers' buffers; this was the
  one split that was ours to remove.)
- UNIVERSAL sizing: `context = "optimal"` and `"auto"` are now the same
  policy -- aim at the 262144 ceiling, let the ladder + fit oracle + placement
  gate settle the largest window that is measurably healthy, and never settle
  below the ~100k bottom target (BottomCtxTokens = 98304: ~64k usable working
  context + the agent's ~24k fixed overhead + the compaction reserve) while a
  slower path exists. When full-GPU residency can't reach the bottom, the
  launcher trades in cost order: first drop the MTP draft (frees its context +
  buffers, ~1-2 GB at big windows, costs ~25-35% decode, stays fully resident
  and gate-verified); only then fall back to the engine's device placement
  (layers spill to RAM, measured 2-4x decode). The decode report states what
  was traded; the launch memo records the placement (gpu/nomtp/spill) so
  replays load the same way instead of being gate-rejected every start.
- The starved-KV downshift now reads the RAW full-GPU estimate instead of the
  bottom-bumped target -- the bump would have hidden starvation and silently
  disabled the asymmetric q8_0/q4_0 downshift on exactly the cards it exists
  for.

### Added
- Auto-continuation (cloud parity): a response that stops at max_tokens
  mid-TEXT is continued by the router itself -- the partial answer goes back
  to the same backend as an assistant prefill (the /v1/messages contract:
  a trailing assistant message is resumed, not answered) and the continuation
  is spliced into the SAME client response, streaming and non-streaming alike,
  up to 2 legs. The agent receives ONE complete message instead of a half
  answer it treats as final -- the local models' worker caps and small windows
  hit this constantly where the cloud rarely does. Never applies to tool_use/
  thinking cuts (no prefill form exists), tiny-cap probe requests, or OpenAI-
  shape paths. The client still receives every token, so its transcript and
  token accounting stay truthful. Session stats: `answers-continued=N`.

### Notes (the 28 GB question, answered with the real ledger)
- "28.6 GB minus a 19.8 GB model" is not 8.8 GB of context: ~1-2 GB of
  desktop/driver use was already on the cards, each GPU allocates its own
  compute buffers (~1-2.5 GB combined at -b 2048), the MTP draft keeps its own
  context + buffers (~1.5-2 GB at big windows -- it is what OOM'd the smaller
  card at 131k), and a dual-GPU split strands what the per-card ratio can't
  use. The fit-oracle skips in the launch log were echoing a measured truth
  (98304-with-MTP was sysmem-paged at 89 tok/s two days ago); what changed in
  this release is the POLICY above them.

## v1.18.1 — 2026-06-11

### Fixed
- `winc setup` gated its PATH step on the RECORDED rc entries, so an install
  that wrote .bashrc before fish support existed "looked recorded" forever and
  setup never repaired it -- the exact installs that needed the fish fix.
  Setup now gates on the LIVE PATH (same as `winc update`) and re-applies the
  idempotent AddToPath, which fills precisely the gaps: the fish conf.d
  drop-in, the ~/.local/bin symlink, any missing rc lines. install.sh's final
  hint no longer assumes bash.

## v1.18.0 — 2026-06-11

Prebuilt installs stop going stale, and 4 GB cards get the 4B.

### Added
- Prebuilt self-update: `winc update` now downloads the latest release binary
  for this OS/arch (sha256-verified against the release's published digests; a
  mismatch is a hard fail and the file is discarded) and swaps it in with the
  same rename dance as the source rebuild. Previously prebuilt installs
  refreshed everything EXCEPT winc itself -- every fix shipped since the
  install simply never arrived unless the user manually re-downloaded, which
  is exactly how a laptop ended up reporting bugs that were already fixed.
- `winc update` re-applies the PATH entry whenever winc isn't reachable from
  the live PATH (the "I have to run ./winc from its folder" state) -- fixes
  installs that recorded PATH before fish support existed.
- PATH on Unix now also gets a ~/.local/bin/winc symlink (on PATH out of the
  box on most modern distros, fish included) alongside the rc-file edits and
  the fish conf.d drop-in. Only ever creates or repoints a symlink -- a user's
  own file at that path is never touched. Uninstall removes it.

### Changed
- The recommendation's runtime headroom now scales with the model instead of a
  flat 2 GB (calibrated on mid-tier models, which keep it): measured on the
  4B-Q4 at full GPU, weights + CUDA/compute overhead + a working KV cache need
  ~1 GB of headroom, not 2. A 4 GB card now gets qwen3.5-4b -- a capable coder
  it demonstrably runs -- instead of stepping down to the 2B.

### Measured (and why no KV-factor change shipped)
- The 4B's KV cache costs ~33 KB/token (measured: two full-GPU loads, 16k vs
  64k windows, VRAM delta) -- HIGHER per token than the big hybrid models the
  64 tokens/MB sizing factor was calibrated on (their Gated-Delta-Net layers
  carry constant-size state; the 4B pays full GQA attention every layer). So
  small models do NOT get a bigger sizing factor: on a 4 GB card the 4B holds
  ~16-24k tokens fully resident, and the v1.17.0 usable-window-via-partial-
  offload behavior is the correct path to an agent-sized 49k window.

## v1.17.1 — 2026-06-11

### Fixed
- The new .winc-hw hardware identity cache is machine-local state and is now
  gitignored (one had been committed alongside v1.17.0; a stale clone's cache
  self-heals at first launch either way).

## v1.17.0 — 2026-06-11

Low-end hardware gets a usable window, and the PATH actually lands on
fish-first distros. A partially offloaded usable window beats a fully resident
useless one.

### Fixed
- 4 GB-class cards collapsed to unusable windows: the sizing reserve was a
  flat 1536 MB calibrated on 20+ GB models (a 4B's real compute buffer is
  ~300 MB), which left the formula negative on a 4 GB card -- and the
  fit-oracle then vetoed every rung that couldn't stay FULLY on GPU, driving
  the launch down to a 16k window, smaller than the agent's own ~24k fixed
  overhead. Three changes:
  - the per-GPU reserve now scales with the model (512 MB + size/8, capped at
    the calibrated 1536 -- models >= 8 GB size exactly as before);
  - when full-GPU sizing still can't reach a workable window, the target
    becomes the 48k usable floor and layer placement falls to the engine's
    device fit (partial offload). The ladder still verifies the load, the
    <49k warning and the decode report tell the user what they got;
  - the fit-oracle only vetoes rungs for forced-full-GPU loads -- a partial
    fit is SUPPOSED to spill, and a failed small-model attempt costs seconds.
  A 4 GB card with the 4B now sizes to 49152 instead of a floor it could
  never use.
- PATH on fish-first distros (CachyOS notably): winc wrote its PATH line to
  .bashrc/.zshrc/.profile -- fish reads none of them. `winc setup` now also
  drops ~/.config/fish/conf.d/winc.fish (fish sources every conf.d file);
  OnPath sees it, uninstall removes it.

## v1.16.0 — 2026-06-11

Lighter on low-end hardware, faster on every relaunch.

### Changed
- Team mode no longer auto-engages at or below the 16 GB-discrete / 24 GB-unified
  hardware class -- including CPU-only boxes, where the old RAM check could
  seat 1-3 extra model servers next to the head on the same cores. The head
  model alone is the right load there. `--team` / `[team] mode = "on"` still
  force a team anywhere.
- Hardware identity is detected once and cached (.winc-hw): launches re-probe
  only what actually moves (per-GPU free memory -- one nvidia-smi call -- and
  total RAM). On Windows non-NVIDIA boxes the identity probes are PowerShell
  invocations costing seconds per launch, every launch, for facts that change
  only on a hardware or driver swap. Self-healing: on NVIDIA the live probe's
  totals must match the cache or a full re-detect runs; elsewhere the cache
  expires after 7 days. `winc detect` / doctor / setup always probe fully and
  refresh the cache.
- The launch memo is now keyed by a config+hardware fingerprint (context mode,
  KV/MoE/MTP knobs, gpu_layers, --parallel slots, VRAM/GPU count): the
  remembered stepping replays only while every sizing input matches, and a
  changed input re-measures ONCE instead of replaying a stale plan (previously
  flipping context "optimal" <-> "auto" kept replaying the old window). One
  entry per geometry, so single-mode and team-mode steppings of the same model
  coexist instead of evicting each other.
- Launches the placement gate doesn't cover (CPU, unified memory, expert
  offload, explicit settings) report decode speed from a tiny completion again
  instead of the gate's ~2.5k-token bench prompt -- that prompt is what makes
  the gate's verdict meaningful, but on a CPU-class box it alone cost a minute
  of startup with nothing to verify.

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
