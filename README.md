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
winc -d qwen2.5-coder-7b           # download one
winc -s claude qwen2.5-coder-7b    # launch Claude Code on it (sandboxed)
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
| `winc -s ... --reasoning <mode>` | Override reasoning mode for this launch |
| `winc serve [--multi]` | Run the server(s)/router only (point your own client at it) |
| `winc -c` / `winc check` | Show latest engine versions |
| `winc -u` / `winc update` | Refresh engine binaries (and `git pull` if a clone) |
| `winc -n` / `winc uninstall [-y]` | Remove installed components + PATH entry |
| `winc version` | Print version |

`<model>` is a catalogue alias (see `winc ls`) or any part of a downloaded filename.

---

## One config file: `winc.toml`

Everything lives in `winc.toml` next to the binary — hand-editable, written on first run.

```toml
[general]
default_app   = "claude"
default_model = "qwen2.5-coder-7b"
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
sonnet = "qwen2.5-coder-7b"
opus   = "qwen2.5-coder-7b"
haiku  = "qwen2.5-coder-7b"
```

### Adaptive reasoning

Reasoning models (Qwen3, DeepSeek-R1, ...) can over-think trivial prompts. In **adaptive**
mode (the default) `winc` runs a tiny in-process router that sets a per-request *thinking
ceiling* scaled to request size: "hi" answers instantly, while a real coding task gets a
full budget. Set `mode = "on"` (always think), `"off"` (never), or `"fixed"` for a constant
budget — those run with **zero proxy hop** (direct to llama-server).

---

## Model tiers

`winc ls` groups the catalogue by memory budget (discrete VRAM or Apple unified memory):

| Tier | Target | Examples |
|------|--------|----------|
| `nano` | < 6 GB (phones, weak laptops, iGPUs) | qwen2.5-coder-3b, llama3.2-1b, gemma3-1b, phi4-mini |
| `small` | 6-8 GB | qwen2.5-coder-7b, llama3.1-8b |
| `mid` | 16 GB / 16-32 GB unified | **qwen3.6-35b (MoE)**, gpt-oss-20b, qwen3.6-27b |
| `large` | 24 GB+ / 36 GB+ unified | **qwen3.6-35b-q4 (MoE)**, qwen2.5-coder-32b |
| `xl` | 64 GB+ unified | qwen2.5-72b |

Add your own with a `[[custom_models]]` block in `winc.toml`. Tiers that can fit a
strong MoE coder default to it — a 35B with ~3B active is ~3-5x faster than a dense
27B at near-equal quality.

### Low-end picks (`nano` + `small`), with rough benchmarks

★ = tier default. Figures are Q4_K_M, fully GPU-offloaded, short context, and
**approximate** — HumanEval varies by eval harness (use it for relative ranking),
and tok/s swings with GPU / quant / context. Run `winc detect` to see what your
machine resolves to.

**`nano` — <6 GB (2-4 GB GPUs, iGPUs, phones)**

| Model | Params | VRAM | HumanEval~ | tok/s (4-6 GB GPU / CPU) | Best for |
|---|---|---|---|---|---|
| ★ qwen2.5-coder-3b | 3B | 1.9 GB | ~84% | ~50-70 / ~12-20 | best tiny **coder** |
| qwen2.5-coder-1.5b | 1.5B | 1.0 GB | ~70% | ~90-120 / ~25-40 | fastest coder (~3 GB) |
| phi4-mini | 3.8B | 2.5 GB | ~70% | ~45-65 / ~10-18 | math / logic |
| llama3.2-3b | 3B | 2.0 GB | ~51% | ~55-75 / ~12-22 | general chat |
| smollm2-1.7b | 1.7B | 1.1 GB | ~25% | ~90-120 / ~25-40 | ultra-light |
| llama3.2-1b | 1B | 0.8 GB | ~18% | ~120-160 / ~40-60 | phones / edge |
| gemma3-1b | 1B | 0.8 GB | ~15% | ~120-160 / ~40-60 | flash chat |

**`small` — 6-8 GB (GTX 1660 / RTX 3050 / RX 6600-class)**

| Model | Params | VRAM | HumanEval~ | tok/s (6-8 GB GPU / CPU) | Best for |
|---|---|---|---|---|---|
| ★ qwen2.5-coder-7b | 7B | 4.7 GB | ~88% | ~25-35 / ~6-10 | best small **coder** |
| deepseek-r1-8b | 8B | 5.0 GB | strong* | ~25-32 / ~6-10 | math/algorithmic (*reasoning) |
| llama3.1-8b | 8B | 4.9 GB | ~72% | ~25-32 / ~6-10 | well-rounded general |
| gemma4-e4b | ~4B eff | 5.0 GB | modest | ~35-50 / ~10-16 | newest, multimodal |

Anchors: an RTX 3050 8 GB runs an 8B Q4 at ~28 tok/s; a 3B is ~2x that, a 1B ~4-5x;
GPU is ~5-10x faster than CPU. **For coding at any size, prefer the Qwen2.5-Coder line.**

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
| **MoE expert offload** | For a Mixture-of-Experts model that won't fit VRAM, keeps **attention on the GPU** but parks the **expert weights in RAM** (`--cpu-moe`) instead of dropping whole layers | A 35B-A3B MoE runs near GPU speed on 12 GB: only the ~3B active params' activations cross PCIe, not 35B of weights |
| **Flash attention + Q8 KV cache** | Enables `--flash-attn` and stores the KV cache at `q8_0` when on GPU | Halves KV-cache memory → bigger context in the same VRAM, and faster attention |
| **VRAM-aware context + silent retry** | Sizes the context window to the VRAM left after the model — scaled to the KV `cache_type` (`q4_0` fits ~2× the tokens of `q8_0`) — then **steps down a ladder** (128k → 96k → 64k → … → 16k) if the first choice doesn't load | You get the largest context that fits — and never see an out-of-memory error on launch |
| **Prefix KV caching** | Built into llama-server: the static system-prompt + tools KV is **reused across turns** | Claude Code's ~25k-token system prompt is processed once, not on every message |
| **Adaptive reasoning** | A per-request *thinking ceiling* scaled to request size (see [Adaptive reasoning](#adaptive-reasoning)) | "hi" answers instantly instead of burning a 4k-token think budget |
| **MoE-first model picks** | The `mid`/`large` tier defaults are MoE coders (e.g. qwen3.6-35b-A3B) | ~3-5x the tok/s of a same-size dense model at near-equal quality |
| **Auto-paired draft (dense)** | Downloading a **dense** catalogue model offers its tiny same-tokenizer draft; once present, `winc` enables `--spec-draft-model` automatically at launch. MoE models are skipped (drafts backfire there) | The draft proposes tokens the big model verifies in a batch — up to ~2× on predictable code |
| **Batch / ubatch tuning** | Sets `-b 2048 -ub 512` when offloading to GPU | Faster prompt processing (the "reading your repo" phase) |

### MoE expert offload, in one line

A **Mixture-of-Experts** model has many "expert" sub-networks but only activates a few
per token. `winc` exploits that: attention layers (which every token uses) stay on the
GPU, while the big, rarely-touched expert weights live in RAM. The result — a model
whose weights are **larger than your VRAM** still runs at close to full-GPU speed,
because each token only moves a small activation vector across the bus, not gigabytes of
experts. This is why the `mid` tier recommends a 35B MoE even on a 16 GB card.

`winc` turns it on **only when a MoE model won't otherwise fit** (a model that fits VRAM
stays 100% on the GPU, which is still fastest). Override with `cpu_moe` below.

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

# Escape hatch: any extra llama-server flags, appended verbatim.
extra_server_args = []   # e.g. ["--mlock"] (lock model in RAM) or ["--n-cpu-moe", "16"]
```

**Speculative decoding is automatic for dense models.** When you download a dense
catalogue model, `winc` offers to also grab its tiny same-tokenizer draft (a 0.5B coder
for the Qwen2.5-Coder models, Qwen3-0.6B for the Qwen3 dense models, Llama-3.2-1B for
Llama-3.1-8B). Once the draft is present, `winc` turns on `--spec-draft-model`
automatically at launch — up to ~2× on predictable code. **MoE models are never paired**
(only ~3B is active, so a draft just adds overhead). `draft_model` forces a specific
draft or overrides the auto-pick.

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
