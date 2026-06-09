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
(CUDA / Metal / Vulkan / ROCm / CPU) + llama-swap, picks a model for your memory tier, and adds
`winc` to your PATH. It's idempotent and fully portable — move the folder anywhere and it still
works (no baked paths).

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
| `winc -d <alias> [-y]` | Download a catalogue model (offers its speculative-decoding draft for dense models; `-y` auto-accepts) |
| `winc -d <repo> <file>` | Download any GGUF from HuggingFace |
| `winc -r <model> [-y]` | Delete a downloaded model |
| `winc -s claude <model>` | Start Claude Code on a local model (sandboxed instance) |
| `winc -s opencode <model>` | Start OpenCode |
| `winc -s openclaw <model>` | Start OpenClaw |
| `winc -s cli <model>` | Raw llama.cpp chat |
| `winc -s ... --multi` | Route through llama-swap (multiple models, hot-swapped) |
| `winc -s claude <model>` | **Team is the default on a big model**: it orchestrates while a small CPU worker runs all subagents (research fan-out + Explore) |
| `winc -s ... --noteam` | Disable team mode — run a single model |
| `winc -s ... --reasoning <mode>` | Override reasoning mode for this launch |
| `winc serve [--multi]` | Run the server(s)/router only (point your own client at it) |
| `winc -c` / `winc check` | Update status: winc version, source freshness, engine, catalog |
| `winc -u` / `winc update` | Update **everything**: pull + rebuild (clone), refresh engine binaries + model catalog |
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
parallel  = 4               # concurrent slots on the haiku/mid workers
```

### Adaptive reasoning

Reasoning models (Qwen3.5, Qwen3.6, ...) can over-think trivial prompts. In **adaptive**
mode (the default) `winc` runs a tiny in-process router that sets a per-request *thinking
ceiling* scaled to request size: "hi" answers instantly, while a real coding task gets a
full budget. Set `mode = "on"` (always think), `"off"` (never), or `"fixed"` for a constant
budget — those run with **zero proxy hop** (direct to llama-server).

### Agent team (default on a big model)

Normally every subagent Claude Code spawns — and every agent a multi-agent **Workflow**
fans out — is a clone of the model you launched, so a deep-research fan-out runs N copies of
your big model: slow and wasteful. On a big model **winc runs team mode by default**: the
launched model stays the **main orchestrator** while small workers run alongside it on the
**CPU** (never touching the main model's VRAM or context) and handle the subagents.
`--noteam` runs a single model; small main models stay single automatically.

By default (`subagents = "dynamic"`) every subagent — Task tool **and** Workflow fan-out — is
tagged onto the workers and **starts on the 0.8B, escalating by request load** through the
**2B** (light research), the **4B** (medium), and — only when the GPU has VRAM headroom to
spare — the main model itself. The swarm starts cheap and grows only as the work demands —
infra-driven and deterministic, not left to the small model's judgment. Set `mid = "off"` to
drop the 2B rung (0.8B→4B→main), or point it at a different model.

| `subagents` | Behavior |
|-------------|----------|
| `dynamic` *(default)* | start on 0.8B, escalate 0.8B→2B→4B→(main, VRAM permitting) by load |
| `haiku` | force everything to the 0.8B (cheapest, no escalation) |
| `sonnet` | force everything to the 4B |
| `tiered` | per-agent pins (research→0.8B, collator/review→4B); generic/Workflow agents inherit main |

Escalation to the **main** model only happens when there's genuine VRAM headroom (else it
caps at the 4B), so the orchestrator stays responsive. Research-tier calls run with a brief,
**capped** thinking budget (small models call tools far more reliably with a little thinking
than none, but unbounded thinking is slow and can trap the call in the reasoning block).
Nano/small models also get loop-safe, family-appropriate sampling automatically. **Web
search/fetch and read-only tools are pre-approved** in winc's sandbox, so you're never
prompted to grant them every launch. `winc` offers to download a missing worker and ships
ready-made `research`, `collator`, and `code-reviewer` agents — your project `.claude/agents`
always win.

---

## Model tiers

`winc ls` groups the catalogue by memory budget (discrete VRAM or Apple unified memory):

| Tier | Target | Examples (2026 roster) |
|------|--------|----------|
| `nano` | < 6 GB (phones, weak laptops, iGPUs) | **qwen3.5-4b**, gemma4-e2b, qwen3.5-2b, qwen3.5-0.8b |
| `small` | 6-8 GB / 8-16 GB unified | **qwen3.5-9b**, omnicoder-9b, gemma4-e4b |
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
(Prebuilt installs can't self-rebuild — redownload the release for code changes; the
catalogue + engine still refresh.)

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
| **Driver-aware backend** | Picks the right prebuilt llama.cpp build — CUDA 13.x vs 12.x by your **driver** version, else Metal / Vulkan / ROCm / CPU — and **falls back at runtime** (cuda → vulkan → cpu) if a backend won't actually run | A CUDA build that matches your driver runs on the GPU instead of silently dropping to CPU |
| **GPU layer auto-fit** | Leaves `-ngl` on auto so llama.cpp packs as many layers into VRAM as fit (partial offload when a model is bigger than VRAM) | Every layer on the GPU is ~5-10x faster than on the CPU |
| **MoE expert offload** | For a Mixture-of-Experts model that won't fit VRAM — **or fits so tightly it leaves no room for context** — keeps attention + MTP heads on the GPU but parks the **expert weights in RAM** (`--cpu-moe`), freeing VRAM for a much larger context | A 35B-A3B runs near GPU speed on 12 GB; on a tight 16 GB fit it trades a little speed for a ~32k→~100k+ context |
| **Tell Claude Code the real context** | Passes the loaded window to Claude Code (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`) so its auto-compaction fires at 85% of the *local* size, not the ~200k it assumes for cloud Sonnet | No more `400 … exceeds the available context size` mid-session — Claude Code compacts in time |
| **Flash attention + Q8 KV cache** | Enables `--flash-attn` and stores the KV cache at `q8_0` when on GPU | Halves KV-cache memory → bigger context in the same VRAM, and faster attention |
| **VRAM-aware context + silent retry** | Sizes the context window to the VRAM left after the model — scaled to the KV `cache_type` (`q4_0` fits ~2× the tokens of `q8_0`) — then **steps down a ladder** (128k → 96k → 64k → … → 16k) if the first choice doesn't load | You get the largest context that fits — and never see an out-of-memory error on launch |
| **Prefix KV caching** | Built into llama-server: the static system-prompt + tools KV is **reused across turns** | Claude Code's ~25k-token system prompt is processed once, not on every message |
| **Adaptive reasoning** | A per-request *thinking ceiling* scaled to request size (see [Adaptive reasoning](#adaptive-reasoning)) | "hi" answers instantly instead of burning a 4k-token think budget |
| **MoE-first model picks** | The `mid`/`large` tier defaults are MoE coders (e.g. qwen3.6-35b-A3B) | ~3-5x the tok/s of a same-size dense model at near-equal quality |
| **Auto-paired draft (dense)** | Downloading a **dense** catalogue model offers its tiny same-tokenizer draft; once present, `winc` enables `--spec-draft-model` automatically at launch. MoE models are skipped (drafts backfire there) | The draft proposes tokens the big model verifies in a batch — up to ~2× on predictable code |
| **MTP (Qwen3.6 variants)** | The `*-mtp` model variants carry built-in multi-token-prediction heads; `winc` auto-adds `--spec-type draft-mtp` when one is loaded (and the engine supports it — probed, never breaks an old engine) | ~1.4–2.2× on the dense 27B, ~1.15–1.25× on the 35B MoE — the **only** speculative win for that MoE |
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
context    = "auto"   # "auto" (size to VRAM, with fallback) or a fixed token count
batch      = "auto"   # "auto" (2048) or an integer
flash_attn = true     # flash attention when on GPU
cache_type = "q8_0"   # KV cache: q8_0 (default) | f16 (max quality) | q4_0 (smallest → ~2× the auto context). Needs flash_attn
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

**MTP for the Qwen3.6 MoE (`*-mtp` variants).** Qwen3.6 changed tokenizers, so a separate
draft model is the *wrong* tool — and classic drafts actually regress it. The right lever
is **Multi-Token Prediction**: prediction heads baked into the model, no second model.
Grab the MTP build — `winc -d qwen3.6-35b-mtp` (the IQ3_S 35B MoE) or `winc -d
qwen3.6-35b-q4-mtp` (the Q4 build for 24 GB+ GPUs / 32 GB+ Macs) — and `winc` adds
`--spec-type draft-mtp` automatically when it loads (~1.5–2× — the only speculative
speedup that helps a MoE), **after probing that your engine supports the flag** (older
engines just run without it). `mtp = "off"` disables it.

**More context at ~the same speed:** set `cache_type = "q4_0"` to halve KV-cache bytes per
token — `winc`'s auto-context sizing then fits roughly **2× the tokens** in the same VRAM
(up to the model's trained limit). `q8_0` stays the default for the best speed/accuracy
balance.

Run `winc detect` to see exactly what `winc` resolved for your machine (backend, VRAM,
context, MoE offload, recommended tier).

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
