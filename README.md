# winc.cpp

Run **Claude Code**, **OpenCode**, or **OpenClaw** against a fully local LLM — no API costs, nothing leaves your machine. One small **Go binary**, cross-platform (Windows, Linux, macOS), no Python and no PowerShell.

`winc` wires together:

| Component | Role |
|-----------|------|
| [llama.cpp](https://github.com/ggml-org/llama.cpp) (`llama-server`) | Runs the local GGUF model and **serves the Anthropic Messages API natively** (`/v1/messages`, `--jinja` tools) |
| [llama-swap](https://github.com/mostlygeek/llama-swap) | Optional: hot-swaps between multiple local models behind one endpoint |
| `winc` | Detects hardware, fetches the engine + models, launches the agent pointed at the local server |

Claude Code thinks it's talking to Anthropic. It's actually talking to your GPU — **with no translation proxy** (llama-server speaks Anthropic directly).

---

## Quick start

1. Get `winc` for your OS — download the prebuilt binary from Releases, **or** build from source (`go build -o winc ./cmd/winc`).
2. Run the one-click setup:
   - **Windows:** double-click `install.cmd` (or run `winc setup`)
   - **macOS:** double-click `install.command` (or run `./winc setup`)
   - **Linux:** run `./install.sh` (or `./winc setup`)
3. Open a new terminal and start coding on a local model:

```
winc ls                      # list downloaded + available models
winc -d qwen2.5-coder-7b      # download one
winc -s claude qwen2.5-coder-7b   # launch Claude Code on it (sandboxed)
```

`winc setup` detects your hardware, downloads the right prebuilt llama.cpp backend
(CUDA / Metal / Vulkan / ROCm / CPU) + llama-swap, picks a model for your memory tier,
and adds `winc` to your PATH. It is idempotent and fully portable — move the folder
anywhere and it still works (no baked paths).

---

## Commands

| Command | What it does |
|---------|--------------|
| `winc setup` | First-run wizard: detect -> engine -> model -> PATH |
| `winc ls` | Downloaded models, then the catalogue (tiered, `[installed]` marked) |
| `winc -d <alias>` | Download a catalogue model |
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
| `mid` | 16 GB / 16-32 GB unified | qwen3.6-27b, gpt-oss-20b, devstral |
| `large` | 24 GB+ / 36 GB+ unified | qwen2.5-coder-32b, qwen3-32b |
| `xl` | 64 GB+ unified | qwen2.5-72b |

Add your own with a `[[custom_models]]` block in `winc.toml`.

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

Requires Go 1.22+. The only runtime dependency is the engine (`llama-server`, optionally
`llama-swap`), which `winc setup` downloads as prebuilt binaries into `bin/`.

---

Author: Sam Dotson. llama.cpp and llama-swap are separate upstream projects under their own licenses.
