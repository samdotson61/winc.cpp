# winc.cpp

Run **Claude Code**, **OpenCode**, or **OpenClaw** against a fully local LLM — no API costs, nothing leaves your machine. One small **Go binary**, cross-platform (Windows, Linux, macOS), no Python and no PowerShell.

*My own rebuild — written from scratch in Go, **not a fork or port** of anything. It's inspired by the methods of earlier local-model launchers; see [Origin & credits](#origin--credits).*

`winc` wires together:

| Component | Role |
|-----------|------|
| [llama.cpp](https://github.com/ggml-org/llama.cpp) (`llama-server`) | Runs the local GGUF model and **serves the Anthropic Messages API natively** (`/v1/messages`, `--jinja` tools) |
| [llama-swap](https://github.com/mostlygeek/llama-swap) | Optional: hot-swaps between multiple local models behind one endpoint |
| `winc` | Detects hardware, fetches the engine + models, launches the agent pointed at the local server |

Claude Code thinks it's talking to Anthropic. It's actually talking to your GPU — **with no translation proxy** (llama-server speaks Anthropic directly).

---

## Quick start

**From a clone — no prerequisites. The installer builds `winc` for you, installing Go automatically if it's missing:**

```
git clone <this-repo-url> winc.cpp
cd winc.cpp
```
- **Windows:** double-click `install.cmd`
- **macOS:** double-click `install.command`
- **Linux:** run `./install.sh`

**Or grab a prebuilt binary** (once a release is published): download `winc` for your OS from the
repo's **Releases**, put it in a folder, and run `winc setup` — no build, no Go.

Either way, `winc setup` detects your hardware, downloads the right prebuilt llama.cpp backend
(CUDA / Metal / Vulkan / ROCm / CPU — with **native ARM builds** on Windows-on-ARM and ARM
Linux, Adreno OpenCL included, never x64-under-emulation) + llama-swap, picks a model for your
memory tier, and adds `winc` to your PATH. It's idempotent and fully portable — move the folder
anywhere and it still works (no baked paths).

Then start coding on a local model:

```
winc ls                            # list downloaded + available models
winc -d qwen3.5-9b           # download one
winc -s claude qwen3.5-9b    # launch Claude Code on it (sandboxed)
```

---

## Commands

| Command | What it does |
|---------|--------------|
| `winc setup` | First-run wizard: detect -> engine -> model -> PATH |
| `winc ls` | Downloaded models, then the catalogue (tiered, `[installed]` marked) |
| `winc -d <alias> [-y]` | Download a catalogue model (offers its speculative-decoding draft for dense models and the MTP head for Gemma 4; `-y` auto-accepts) |
| `winc -d <repo> <file>` | Download any GGUF from HuggingFace |
| `winc -r <model> [-y]` | Delete a downloaded model |
| `winc -s` | Start the **last used** agent on the **last used** model (every successful agent start updates the defaults) |
| `winc -s claude <model>` | Start Claude Code on a local model (sandboxed instance) |
| `winc -s opencode <model>` | Start OpenCode |
| `winc -s openclaw <model>` | Start OpenClaw |
| `winc -s cli <model>` | Raw llama.cpp chat (doesn't change the defaults) |
| `winc -s ... --multi` | Route through llama-swap (multiple models, hot-swapped) |
| `winc -s claude <model>` | **Team is the default on a big model**: it orchestrates while a small CPU worker runs all subagents (research fan-out + Explore) |
| `winc -s ... --noteam` | Disable team mode — run a single model |
| `winc -s ... --reasoning <mode>` | Override reasoning mode for this launch |
| `winc serve [--multi]` | Run the server(s)/router only (point your own client at it) |
| `winc serve/-s ... --journal[=off]` | Override the journal (context virtualization) for this run |
| `winc journal [ls\|show\|rm\|path]` | Inspect the conversation journal — plaintext, local, nothing auto-deleted |
| `winc doctor` | Read-only health snapshot: hardware, engine, models (GGUF check), config, agents, ports, logs |
| `winc logs [name] [--bundle]` | Show log tails; `--bundle` zips a support archive for bug reports |
| `winc -c` / `winc check` | Update status: winc version, source freshness, engine, catalog |
| `winc -u` / `winc update` | Update **everything**: pull + rebuild (clone), refresh engine + catalog, and reconcile `winc.toml` (repair a stale `default_model`, add new config sections) |
| `winc -n` / `winc uninstall [-y]` | Remove installed components + PATH entry |
| `winc version` | Print version |

`<model>` is a catalogue alias (see `winc ls`) or any part of a downloaded filename.

---

## One config file: `winc.toml`

Everything lives in `winc.toml` next to the binary — hand-editable, written on first run.

```toml
[general]
default_app   = "claude"
default_model = "qwen3.5-9b"
port = 8080

[reasoning]
mode = "adaptive"          # adaptive | on | off | fixed
fixed_budget_tokens = 2048

[reasoning.adaptive]        # snappy for small requests, full thinking for big tasks
tiers = [
  { max_input_tokens = 64,    budget_tokens = 0    },
  { max_input_tokens = 512,   budget_tokens = 512  },
  { max_input_tokens = 4000,  budget_tokens = 2048 },
  { max_input_tokens = 16000, budget_tokens = 4096 },
]
ceiling_budget_tokens = 8192
complexity_boost = true     # +1 tier for code / tool-use / build-intent prompts

[journal]                   # context virtualization: long chats stay fast (see below)
enabled = true              # engages only on small windows (< 48k) with an "auto" budget
budget_tokens = "auto"      # live prompt target; auto = clamp(context/2, 2048, 8192)
recall_tokens = 800         # cap on recalled text injected per request
recall_top_k = 4            # evicted turns recalled per request (0 = trim-only)

[performance]
backend = "auto"            # auto | cuda | metal | vulkan | rocm | cpu
gpu_layers = "auto"
flash_attn = true
cache_type = "q8_0"

[multi]                     # llama-swap model-slot mapping (winc -s ... --multi)
sonnet = "qwen3.5-9b"
opus   = "qwen3.5-9b"
haiku  = "qwen3.5-9b"

[team]                      # agent hierarchy — DEFAULT for a big model (--noteam to disable)
mode      = "auto"          # auto (team for big models) | on | off
subagents = "dynamic"       # dynamic (start small, escalate by load) | haiku | sonnet | tiered
sonnet    = "qwen3.5-4b"    # the "sonnet" worker model (escalation target)
mid       = "qwen3.5-2b"    # dynamic-mode middle rung between 0.8B and 4B ("off" to disable)
haiku     = "qwen3.5-0.8b"  # the "haiku" worker model (default subagent / research)
parallel  = 4               # concurrent slots on the haiku/mid workers (halved on <=16GB RAM)
worker_tools = ["WebSearch","WebFetch","Read","Grep","Glob"]          # tools the 0.8B/2B may use
sonnet_tools = ["WebSearch","WebFetch","Read","Grep","Glob","Write"]  # 4B also gets Write; ["all"]=no strip
```

### Adaptive reasoning

Reasoning models (Qwen3.5, Qwen3.6, ...) can over-think trivial prompts. In **adaptive**
mode (the default) `winc` runs a tiny in-process router that sets a per-request *thinking
ceiling* scaled to request size: "hi" answers instantly, while a real coding task gets a
full budget. Set `mode = "on"` (always think), `"off"` (never), or `"fixed"` for a constant
budget — those run with **zero proxy hop** (direct to llama-server).

### Journal — long chats on a small live prompt (context virtualization)

Small local models degrade — and slow down — as a conversation grows: the KV cache eats RAM,
prefill eats time, and a 4B attending over 30k tokens of history answers worse than one
reading a focused 4k prompt. The journal is **on by default where it helps**: with an
`"auto"` budget it engages only when the loaded context is genuinely small (under ~48k
tokens) and stays dormant on big windows — virtualizing a 128k window down to 8k would
trade capability your hardware has for savings it doesn't need. (Set a numeric
`budget_tokens` to force it on any window; `--journal=off` or `enabled = false` kills it.)
When engaged, `winc` keeps the **live prompt at a fixed budget** no matter how long the
chat gets:

- **Evict:** old turns leave the forwarded prompt and land in a per-conversation store —
  verbatim, human-readable JSONL under `<install>/journal/` (files are truth; summaries
  never replace the record).
- **Recall:** each request, the most relevant evicted turns (BM25 + recency, capped at
  `recall_tokens`) are injected back as a clearly-labeled historical block. Ask about the
  locker code from 60 turns ago and the original turn comes back with the question.
- **Summarize:** on eviction batches a small rolling summary is refreshed as a gist
  safety-net for what recall might miss (`summary_tokens = 0` disables).

Clients keep sending full history to the same endpoint — **no client changes**; `winc`
virtualizes transparently, and conversations are re-identified across restarts by their
own content (no session IDs). Every touched response carries an `X-Winc-Journal` header
(`conv=… recalled=… evicted=… live=…`) and `winc-journal.log` gets one line per request,
so what was recalled is checkable — never vibes. `winc journal show <id>` is the notebook
view; `winc journal rm <id>` is the **only** deletion path.

**Privacy, plainly:** transcripts are stored as **plaintext on your machine**. That's the
product — offline, local, private — but know it's there and where it lives
(`winc journal path`).

### Agent team (default on a big model)

Normally every subagent Claude Code spawns — and every agent a multi-agent **Workflow**
fans out — is a clone of the model you launched, so a deep-research fan-out runs N copies of
your big model: slow and wasteful. **winc runs team mode by default on hardware above the
16 GB-discrete / 24 GB-unified class, for any main model above the nano tier when there's
enough system RAM for the workers**: the launched model stays the **main orchestrator**
while small workers run alongside it and handle the subagents. At or below that hardware
class (including CPU-only boxes) the head model alone is the right load — auto mode stays
single, and `--team` (or `[team] mode = "on"`) still forces a team when you want one. **The
head takes VRAM precedence absolutely** — it loads first and takes everything it wants; the
workers then claim only the **measured leftover VRAM** (largest worker first, with a CPU
fallback if the load doesn't fit after all) and otherwise run on the **CPU**, so they can
never shrink the head's context. The worker set is **fit to available
RAM** — smallest-first, dropping the largest first, down to just the 0.8B on a tight box —
and only falls back to a single model when not even the smallest worker fits. On a
small-RAM box (≤16GB) winc also **halves each worker's parallel slots while keeping its
context window**, so every agent gets double the context — fewer overflows, fewer
escalations, and less CPU contention on the hardware that feels them most. `--noteam`
forces a single model; a nano main model stays single automatically.

By default (`subagents = "dynamic"`) every subagent — Task tool **and** Workflow fan-out — is
tagged onto the workers and **starts on the 0.8B, escalating by request load** through the
**2B** (light research), the **4B** (medium), and — only when the GPU has VRAM headroom to
spare — the main model itself. The swarm starts cheap and grows only as the work demands —
infra-driven and deterministic, not left to the small model's judgment. Set `mid = "off"` to
drop the 2B rung (0.8B→4B→main), or point it at a different model.

| `subagents` | Behavior |
|-------------|----------|
| `dynamic` *(default)* | start on 0.8B, escalate 0.8B→2B→4B→(main, VRAM permitting) by load; read/search/fetch-only agents cap at the 4B |
| `haiku` | force everything to the 0.8B (cheapest, no escalation) |
| `sonnet` | force everything to the 4B |
| `tiered` | per-agent pins (research→0.8B, collator/review→4B); generic/Workflow agents inherit main |

Escalation to the **main** model only happens when there's genuine VRAM headroom (else it
caps at the 4B), so the orchestrator stays responsive — and it is reserved for subagents
that can actually **act**. An **information-only request** (every tool it carries is
read/search/fetch — an explorer, a researcher, a fetcher — or it carries no tools at all)
**never reaches the main model**, no matter how large its context grows: it tops out at the
largest worker instead. A second full session on the big GPU model is strictly slower than
a worker for read-and-report work, and such a request has no tool that could use the head's
extra capability. The end-of-session stats count these as `info-pinned`. Research-tier calls run with a brief,
**capped** thinking budget (small models call tools far more reliably with a little thinking
than none, but unbounded thinking is slow and can trap the call in the reasoning block).
Nano/small models also get loop-safe, family-appropriate sampling automatically. **Web
search/fetch and read-only tools are pre-approved** in winc's sandbox, so you're never
prompted to grant them every launch. `winc` offers to download a missing worker and ships
ready-made `research`, `collator`, and `code-reviewer` agents — your project `.claude/agents`
always win.

**Prompt compaction.** Worker requests are trimmed to a **per-tier tool allowlist** (the
tools array, not the system prose, is what bloats a request): the 0.8B/2B get a research-only
set, the 4B also gets `Write`, and the HEAD model keeps every tool. This is the bulk of the
savings — fewer tools means a smaller prefill (faster first token, fewer stream-idle
timeouts) and better tool-selection on tiny models. Stripping is deterministic per tier, so
llama.cpp's prompt-prefix cache still reuses the worker's head across the fan-out; `winc` also
adds `--cache-reuse` (probed) and losslessly drops optional `input_examples`. Set
`worker_tools` / `sonnet_tools` in `[team]` (`["all"]` disables stripping for a tier).

---

## Model tiers

`winc ls` groups the catalogue by memory budget (discrete VRAM or Apple unified memory):

| Tier | Target | Examples (2026 roster) |
|------|--------|----------|
| `nano` | < 6 GB (phones, weak laptops, iGPUs) | **qwen3.5-4b**, gemma4-e2b, qwen3.5-2b, qwen3.5-0.8b |
| `small` | 6-8 GB / 8-16 GB unified | **qwen3.5-9b**, omnicoder-9b, gemma4-12b, gemma4-e4b |
| `mid` | 16 GB / ~24 GB unified | **qwen3.6-35b (MoE)**, gemma4-26b-a4b, qwen3.6-27b |
| `large` | 24 GB+ / 32-64 GB unified | **qwen3.6-35b-q4 (MoE)**, gemma4-26b-a4b-q4, qwen3.6-27b-q5 |
| `xl` | 96 GB+ unified | qwen3-coder-next-80b, mistral-small4-119b |

The catalogue advertises **only models released in 2026** — the Qwen3.5 / Qwen3.6 and
Gemma 4 lines, with full tool-calling down to 0.8B. It's refreshed over time via
`winc update` (see below), so older rosters are pruned as better models land.

> **Apple Silicon note:** unified memory is shared with the OS and the GPU can only use
> ~75% of it, so winc budgets ~72% of your RAM when picking a tier — e.g. a 24 GB Mac
> maps to `mid` (a ~14 GB model that fits), not `large` (a 22 GB model that won't).

Add your own with a `[[custom_models]]` block in `winc.toml`. Tiers that can fit a
strong MoE coder default to it — a 35B with ~3B active is ~3-5x faster than a dense
27B at near-equal quality.

`winc update` refreshes the catalogue itself (not just the engine), so **prebuilt-binary
users get newly added models without re-downloading the binary** — the latest catalogue is
fetched and cached to `catalog.json` next to `winc`, which transparently overrides the
built-in one (delete it to revert). The embedded catalogue is the offline fallback.

On a **git clone**, `winc update` goes further: it `git pull`s the whole repo and
**rebuilds the binary**, so *all* source changes land (code, embedded catalogue, fixes) —
not just the engine and catalogue. `winc check` shows whether your source is behind origin.
**Prebuilt installs self-update too**: `winc update` downloads the latest release binary
for your OS/arch (sha256-verified against the release's published digests) and swaps it
in — the next invocation runs the new build. It also re-applies the PATH entry (bash/zsh/
profile, fish conf.d, `~/.local/bin`) whenever `winc` isn't reachable from the live PATH.

`winc update` also **reconciles `winc.toml`** so an update never strands you on a stale
config that silently disables newer defaults: if your `default_model` points at a model
that's no longer in the catalogue (or was never downloaded), it's repointed to the model
recommended for your hardware, and any config sections added since your file was written
(e.g. `[team]`) are appended. It only repairs that one reference and *appends* missing
sections — your existing edits and comments are left untouched. `winc check` flags a stale
`default_model` so you know a repair is pending.

### Low-end picks (`nano` + `small`), 2026 roster

★ = tier default. Figures are Q4_K_M, fully GPU-offloaded, short context, and
**approximate** — benchmarks vary by harness (use them for relative ranking) and tok/s
swings with GPU / quant / context. All of these are **2026 releases with native
tool-calling**, so even the tiny ones can drive an agent (call tools, web search). Run
`winc detect` to see what your machine resolves to.

**`nano` — <6 GB (2-4 GB GPUs, iGPUs, phones)**

| Model | Params | Size | Released | LiveCodeBench~ | tok/s (4-6 GB GPU / CPU) | Best for |
|---|---|---|---|---|---|---|
| ★ qwen3.5-4b | 4B | 2.6 GB | Feb 2026 | ~56 | ~45-65 / ~10-18 | best tiny **coder + tools** |
| gemma4-e2b | 2.3B eff | 2.9 GB | Mar 2026 | ~44 | ~50-70 / ~12-20 | general, multimodal |
| qwen3.5-2b | 2B | 1.2 GB | Feb 2026 | — | ~80-110 / ~22-36 | fastest small coder |
| qwen3.5-0.8b | 0.8B | 0.5 GB | Mar 2026 | — | ~120-160 / ~40-60 | phones / edge / draft |

**`small` — 6-8 GB (RTX 3050 / RX 6600-class) or 8-16 GB unified**

| Model | Params | Size | Released | LiveCodeBench~ | tok/s (6-8 GB GPU / CPU) | Best for |
|---|---|---|---|---|---|---|
| ★ qwen3.5-9b | 9B | 5.7 GB | Mar 2026 | ~66 | ~22-32 / ~6-10 | best small **all-rounder** |
| omnicoder-9b | 9B | 5.9 GB | Mar 2026 | strong | ~22-32 / ~6-10 | agentic **coding** specialist |
| gemma4-12b | 12B | 7.1 GB (Q4) | Jun 2026 | ~72 | ~20-30 / ~5-9 | newest; multimodal generalist (8+ GB) |
| gemma4-e4b | 4.5B eff | 5.0 GB | Apr 2026 | ~52 | ~30-45 / ~9-15 | fast, multimodal |

Anchors: an RTX 3050 8 GB runs a 9B Q4 at ~25 tok/s; a 4B is ~2x that, a sub-1B ~5x+;
GPU is ~5-10x faster than CPU. **For coding, prefer the Qwen3.5 line (or OmniCoder-9B);
all pair with the tiny `qwen3.5-0.8b` draft for speculative decoding.**

---

## Performance

`winc` tunes the engine for you. Everything below is **automatic** — it runs from
`winc detect` (hardware) and the model file, with no config needed. The knobs at the
end of this section only exist if you want to override a decision.

### What `winc` does automatically

| Optimization | What it does | Why it's faster |
|---|---|---|
| **Driver-aware backend** | Picks the right prebuilt llama.cpp build — CUDA 13.x vs 12.x by your **driver** version, else Metal / Vulkan / ROCm / CPU; **ARM machines get native arm64 builds** (Snapdragon: Adreno OpenCL first), never x64 emulation — and **falls back at runtime** (cuda → vulkan → cpu) if a backend won't actually run | A CUDA build that matches your driver runs on the GPU instead of silently dropping to CPU; native ARM prompt speed is a multiple of emulated x64 |
| **P-core thread pinning (CPU inference)** | On a CPU-only install with a known P/E core split (Apple via `sysctl`, ARM/hybrid Linux via cpufreq classes), `--threads` pins to the **performance cores**; unknown split → engine default, never a guess | llama-server's default spans efficiency cores, and on big.LITTLE parts the E cores gate **every** token — each layer waits for the slowest worker |
| **ARM-CPU model rungs** | `-q40` catalogue rungs (`qwen3.5-2b-q40` / `-4b-q40` / `gemma4-e2b-q40`) ship **Q4_0**, which llama.cpp runtime-repacks to dotprod/i8mm layouts on ARM CPUs; they sit beside their K-quant siblings and are never the default | Typically **1.5–2.5× faster prompt processing** on CPU-only ARM (WoA tablets, SBCs) than the K-quant, at a small quality cost the note discloses |
| **GPU layer auto-fit, head-first** | A model that **fully fits** VRAM (after buffers + the MTP draft context) is **forced** fully onto the GPU (`-ngl 99`) — the engine's own fit is conservative and could spill a layer to the CPU on a tight fit. A model that doesn't fit keeps the engine's partial offload | Every layer on the GPU is ~5-10x faster — and one CPU-spilled MoE layer drags **every** token through a CPU expert pass, stealing CPU from the team's workers |
| **Multi-GPU** | Detects **every** GPU (not just the first); the memory budget is the **combined** VRAM, and the engine spreads layers across all cards by each one's free VRAM at load | A 16 GB + 12 GB pair is sized as 28 GB — a 22 GB MoE that needed expert-offload on one card runs **fully on GPU** across two, at a much larger context |
| **MoE expert offload** | For a Mixture-of-Experts model that won't fit VRAM — **or fits so tightly it leaves no room for context** — keeps attention + MTP heads on the GPU but parks the **expert weights in RAM** (`--cpu-moe`), freeing VRAM for a much larger context | A 35B-A3B runs near GPU speed on 12 GB; on a tight 16 GB fit it trades a little speed for a ~32k→~100k+ context |
| **Tell Claude Code the real context** | Passes the loaded window to Claude Code (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`) and sets the compaction trigger to leave **max(8k, window/8) tokens of headroom** — room for the in-flight turn *plus* the compaction summary itself. If a session still overflows, the router **trims the oldest transcript messages out of the compaction request** so the summary completes instead of truncating mid-write | Long sessions auto-compact and keep going; no more overflow → broken-summary → overflow death loop |
| **Flash attention + Q8 KV cache** | Enables `--flash-attn` and stores the KV cache at `q8_0` when on GPU | Halves KV-cache memory → bigger context in the same VRAM, and faster attention |
| **VRAM-aware context + silent retry** | Sizes the context window to the VRAM left after the model — scaled to the KV `cache_type` (`q4_0` fits ~2× the tokens of `q8_0`) — then **steps down a ladder** (256k → 196k → 131k → … → 16k) if the first choice doesn't load. When the window settles short, winc **probes the next rungs with an asymmetric q8_0/q4_0 KV cache** — keys keep full q8 precision (4-bit keys measurably degrade coding), only values compress — applied to the main *and* MTP draft caches, keeping the widest window that loads, memoized per model so the probe cost is paid once | You get the largest context that actually loads — measured on a 16+12 GB pair: the 35B MoE goes **131k → 262k fully on GPU at full speed** |
| **Prefix KV caching** | Built into llama-server: the static system-prompt + tools KV is **reused across turns** | Claude Code's ~25k-token system prompt is processed once, not on every message |
| **Adaptive reasoning** | A per-request *thinking ceiling* scaled to request size (see [Adaptive reasoning](#adaptive-reasoning)) | "hi" answers instantly instead of burning a 4k-token think budget |
| **MoE-first model picks** | The `mid`/`large` tier defaults are MoE coders (e.g. qwen3.6-35b-A3B) | ~3-5x the tok/s of a same-size dense model at near-equal quality |
| **Auto-paired draft (dense)** | Downloading a **dense** catalogue model offers its tiny same-tokenizer draft; once present, `winc` enables `--spec-draft-model` automatically at launch. MoE models are skipped (drafts backfire there) | The draft proposes tokens the big model verifies in a batch — up to ~2× on predictable code |
| **MTP (Qwen variants + Gemma heads)** | Qwen `*-mtp` variants carry built-in multi-token-prediction heads; Gemma 4 models pair with their separately-downloaded MTP head file. `winc` auto-adds the right flags when either is present (engine support probed — never breaks an old engine) | ~1.4–2.2× on the dense Qwen 9B/27B, ~1.15–1.25× on the 35B MoE, ~1.1× on Gemma 26B-A4B |
| **Batch / ubatch tuning** | Sets `-b 2048 -ub 512` when offloading to GPU | Faster prompt processing (the "reading your repo" phase) |

### MoE expert offload, in one line

A **Mixture-of-Experts** model has many "expert" sub-networks but only activates a few
per token. `winc` exploits that: attention layers (which every token uses) stay on the
GPU, while the big, rarely-touched expert weights live in RAM. The result — a model
whose weights are **larger than your VRAM** still runs at close to full-GPU speed,
because each token only moves a small activation vector across the bus, not gigabytes of
experts. This is why the `mid` tier recommends a 35B MoE even on a 16 GB card.

`winc` turns it on automatically when a MoE model **won't fit** *or* fits so tightly that
there's no VRAM left for a usable context (e.g. a 14 GB MTP build on a 16 GB card, which
would otherwise be stuck at the 32k floor). Offloading the experts frees ~10 GB+ for KV,
so the context jumps to ~100k+. Comfortably-fitting models stay 100% on the GPU (fastest).
**Want more context on a tight-fit MoE?** Set `cpu_moe = "on"` — it offloads the experts
(a little slower, MTP softens it) and `winc` then sizes the context to the freed VRAM.

### Tuning knobs — `[performance]` in `winc.toml`

All default to `auto`/off; set them only to override `winc`'s choice.

```toml
[performance]
backend    = "auto"   # auto | cuda | metal | vulkan | rocm | cpu  (force a backend)
gpu_layers = "auto"   # "auto" (fit to VRAM) or an integer -ngl
context    = "optimal"   # "optimal" (~128K/agent, 40-80 tok/s baseline) | "auto" (largest that fits, up to 256K) | a fixed token count
batch      = "auto"   # "auto" (2048) or an integer
flash_attn = true     # flash attention when on GPU
cache_type = "auto"   # KV cache: "auto" (q8_0; drops to q4_0 when the window is starved) | q8_0 | f16 (max quality) | q4_0 (smallest → ~2× the auto context). Needs flash_attn
threads    = "auto"   # CPU threads (auto = all cores)
max_output_tokens = "auto"   # cap on response length ("auto" = ~half the context)

# MoE expert offload (see above)
cpu_moe = "auto"      # "auto" (offload only if it won't fit VRAM) | "on" | "off" | <layer count>

# Speculative decoding: winc AUTO-PAIRS a tiny same-tokenizer draft for dense catalogue
# models once the draft is downloaded (it offers to fetch it on `winc -d <dense>`). Set
# this only to force a specific draft GGUF (must live in models/) or override the pick.
draft_model = ""      # "" = auto-pair for dense models; or a filename to force one

# Multi-Token Prediction: auto-on for *-mtp model variants (built-in draft heads).
mtp = "auto"          # "auto" (on for MTP models, engine permitting) | "off"
mtp_draft_max = 2     # tokens drafted per step (--spec-draft-n-max)

# Escape hatch: any extra llama-server flags, appended verbatim.
extra_server_args = []   # e.g. ["--mlock"] (lock model in RAM) or ["--n-cpu-moe", "16"]
```

**Speculative decoding is automatic for dense models.** When you download a dense
catalogue model, `winc` offers to also grab its tiny same-tokenizer draft (the 0.8B
`qwen3.5-0.8b` pairs with the Qwen3.5 small coders like `qwen3.5-9b` and `omnicoder-9b`).
Once the draft is present, `winc` turns on `--spec-draft-model`
automatically at launch — up to ~2× on predictable code. **MoE models are never paired**
(only ~3B is active, so a draft just adds overhead). `draft_model` forces a specific
draft or overrides the auto-pick.

**MTP (`*-mtp` variants).** Multi-Token Prediction is prediction heads baked into the
model itself — no second model to download or keep in VRAM. For Qwen3.6 it's also the
*only* option: Qwen3.6 changed tokenizers, so a separate draft model is the wrong tool
(classic drafts actually regress it). Grab an MTP build and `winc` adds
`--spec-type draft-mtp` automatically when it loads, **after probing that your engine
supports the flag** (older engines just run without it):

- `winc -d qwen3.6-27b-mtp` / `winc -d qwen3.6-27b-q5-mtp` — the dense 27B coder
  (~1.4–2.2×; the Q5 build for 24 GB+ GPUs / 32 GB+ Macs)
- `winc -d qwen3.6-35b-mtp` / `winc -d qwen3.6-35b-q4-mtp` — the 35B MoE
  (~1.15–1.25× — the only speculative speedup that helps a MoE)
- `winc -d qwen3.5-9b-mtp` — the small-tier 9B, faster than its external 0.8B draft

**Gemma 4 ships its MTP heads as a separate small file** (0.1–0.5 GB) instead of
baking them in. `winc -d <gemma model>` offers the head alongside the model; once
it sits in `models/`, winc pairs it at launch automatically (`--spec-draft-model`
+ `--spec-type draft-mtp`). Measured ~+11% decode on the 26B-A4B with the default
`mtp_draft_max = 2` — higher values lowered acceptance and throughput on consumer
GPUs, so the default stands. Needs an engine from 2026-06-07 or later (probed;
older engines just run without it).

`mtp = "off"` disables it.

**More context at ~the same speed:** this is automatic now — `cache_type = "auto"` drops
the **value** cache to q4_0 (keys stay q8_0; they're the quantization-sensitive side)
whenever the q8_0 window would come up short, and the launch probe verifies the wider
window actually loads. Workers always pin q8_0 — small models suffer most from KV
quantization. Set `cache_type = "q4_0"` (or any `"k/v"` pair) to force a layout —
`winc`'s auto-context sizing then fits proportionally more tokens in the same VRAM
(up to the model's trained limit). `q8_0` stays the default for the best speed/accuracy
balance.

Run `winc detect` to see exactly what `winc` resolved for your machine (backend, VRAM,
context, MoE offload, recommended tier).

---

## Diagnostics & integrity

**When something's off, start with `winc doctor`** — a read-only snapshot of everything
that matters: hardware, engine binaries (checked on disk, never executed), model files
(with a GGUF header check), config, agents on PATH, port status, and which logs exist.
It never starts, stops, or identifies any process. **`winc logs`** prints the tail of any
winc log, and **`winc logs --bundle`** zips a support archive — the doctor report, a
**token-redacted** `winc.toml`, and all logs — ready to attach to a bug report.

**Team sessions watch their workers.** If a worker process dies or stops answering health
checks, the router **reroutes its traffic up the escalation ladder** (pinned routes fall
back to the main model) and notes it in `winc-router.log` — detection only; winc never
kills or restarts anything. After the agent exits, winc prints **session stats**: requests
per backend, context-overflow rewrites, output caps applied, and dead-worker reroutes.

**Downloads are verified.** Engine archives are **sha256-checked** against GitHub's
published release digests before extraction (a mismatch is discarded, never installed;
offline installs proceed with a note). Model downloads get a **GGUF header check**, so an
auth/error page saved as a `.gguf` is caught immediately instead of failing at engine
load — then the file is **sha256-checked** against the digest HuggingFace publishes for
it (skipped with a note if no digest is reachable). A connection that drops mid-download
is detected by length and **resumed** on the next run; the resume is **ETag-validated**
(`If-Range`), so if the repo re-uploaded the file in between, winc restarts cleanly
instead of splicing two versions into one corrupt model. A transfer that goes **silent
for 30s is aborted** with a resumable error rather than hanging forever.
`winc.toml` is written **owner-only** (it can hold your HuggingFace token),
and engine processes are tied to winc's lifetime (Job Objects on Windows, PDEATHSIG on
Linux) so a hard kill can't leave a stray server holding your GPU.

---

## Running alongside cloud Claude Code

`winc -s` sets `CLAUDE_CONFIG_DIR` to a sandboxed `.claude-local/` folder, so your local
instance never touches your logged-in cloud Claude Code. Use Opus in one terminal and a
local model in another.

---

## Build from source

```
go build -o winc ./cmd/winc      # or: make build
make release                      # cross-compile all OS/arch into dist/
go test ./cmd/... ./internal/...
```

Requires Go 1.22+. Dependencies are **vendored** (`vendor/`), so the build needs no network for
modules — only the Go toolchain. The one runtime dependency is the engine (`llama-server`,
optionally `llama-swap`), which `winc setup` downloads as prebuilt binaries into `bin/`.

---

## Origin & credits

winc.cpp is an independent project by **Sam Dotson** — a ground-up rebuild in Go that shares
**no code** with any other tool. It is **not a fork and not a port**. It re-implements an idea
from scratch, taking inspiration from the *methods* of the local-LLM and coding-agent community:

- **[claude.cpp](https://github.com/d4rks1d33/claude.cpp)** (d4rks1d33) — the influence behind
  the core idea of pointing a coding agent at a local model. winc rebuilds that idea with a
  different architecture (single Go binary, native Anthropic serving in llama.cpp, llama-swap
  routing, adaptive reasoning) and a fresh codebase.

It stands on these upstream projects, each under its own license — winc bundles none of them;
`winc setup` downloads them from their official releases:

- **[llama.cpp](https://github.com/ggml-org/llama.cpp)** — local inference engine + native Anthropic Messages API
- **[llama-swap](https://github.com/mostlygeek/llama-swap)** — multi-model routing proxy
- **Claude Code / OpenCode / OpenClaw** — the coding agents winc points at your GPU

---

## License

winc.cpp is released under the **[MIT License](LICENSE)** — Copyright (c) 2026 Sam Dotson.

The engines fetched at runtime (llama.cpp, llama-swap) and the coding agents remain under
their own respective licenses.
