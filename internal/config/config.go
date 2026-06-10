// Package config loads and saves winc.toml -- the single, hand-editable config
// file that holds ALL winc.cpp settings. Everything else (llama-swap.yaml, the
// agent env, server flags) is derived from this. On first run the commented
// default template is written; user edits are preserved (we only ever read it).
package config

import (
	"os"
	"regexp"

	"github.com/pelletier/go-toml/v2"
	"winc/internal/paths"
)

type Config struct {
	General      General       `toml:"general"`
	Reasoning    Reasoning     `toml:"reasoning"`
	Performance  Performance   `toml:"performance"`
	Multi        Multi         `toml:"multi"`
	Team         Team          `toml:"team"`
	HuggingFace  HuggingFace   `toml:"huggingface"`
	Paths        Paths         `toml:"paths"`
	CustomModels []CustomModel `toml:"custom_models"`
}

type General struct {
	DefaultApp   string `toml:"default_app"`
	DefaultModel string `toml:"default_model"`
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
}

type Reasoning struct {
	Mode              string   `toml:"mode"` // adaptive | on | off | fixed
	FixedBudgetTokens int      `toml:"fixed_budget_tokens"`
	Adaptive          Adaptive `toml:"adaptive"`
}

type Adaptive struct {
	Estimate            string       `toml:"estimate"` // chars | count_tokens
	Tiers               []TierBudget `toml:"tiers"`
	CeilingBudgetTokens int          `toml:"ceiling_budget_tokens"`
	ComplexityBoost     bool         `toml:"complexity_boost"`
}

type TierBudget struct {
	MaxInputTokens int `toml:"max_input_tokens"`
	BudgetTokens   int `toml:"budget_tokens"`
}

type Performance struct {
	Backend         string   `toml:"backend"`    // auto | cuda | metal | vulkan | rocm | cpu
	GpuLayers       string   `toml:"gpu_layers"` // "auto" or integer
	Context         string   `toml:"context"`    // "auto" or integer
	Batch           string   `toml:"batch"`      // "auto" or integer
	FlashAttn       bool     `toml:"flash_attn"`
	CacheType       string   `toml:"cache_type"`        // e.g. q8_0, f16
	Threads         string   `toml:"threads"`           // "auto" or integer
	MaxOutputTokens string   `toml:"max_output_tokens"` // "auto" (~half context) or integer
	CpuMoe          string   `toml:"cpu_moe"`           // auto | on | off | <layer count>
	DraftModel      string   `toml:"draft_model"`       // filename of a small draft model (speculative decoding)
	Mtp             string   `toml:"mtp"`               // auto | off  (Multi-Token Prediction for *-MTP models)
	MtpDraftMax     int      `toml:"mtp_draft_max"`     // --spec-draft-n-max for MTP (default 2)
	ExtraServerArgs []string `toml:"extra_server_args"` // advanced: extra llama-server flags
}

type Multi struct {
	Enabled    bool   `toml:"enabled"`
	TTLSeconds int    `toml:"ttl_seconds"`
	Sonnet     string `toml:"sonnet"`
	Opus       string `toml:"opus"`
	Haiku      string `toml:"haiku"`
}

// Team is the heterogeneous agent hierarchy (the default for a big main model): the
// launched model orchestrates as the main agent while a small CPU worker handles ALL
// subagents -- so a deep-research fan-out spins up many quick small agents instead of
// clones of the big model. Mode gates auto-engagement; Subagents picks which worker all
// subagents (Task tool AND the Workflow orchestrator's fan-out) are forced onto.
type Team struct {
	Mode            string   `toml:"mode"`              // auto (team for big main models) | on (always) | off (never)
	Sonnet          string   `toml:"sonnet"`            // the "sonnet" worker model (collator / code-review)
	Mid             string   `toml:"mid"`               // optional middle rung for dynamic mode (e.g. the 2B); "off" disables
	Haiku           string   `toml:"haiku"`             // the "haiku" worker model (research fan-out + Explore)
	Parallel        int      `toml:"parallel"`          // concurrent slots on the worker (research fan-out width); halved on <=16GB-RAM systems
	Subagents       string   `toml:"subagents"`         // which worker ALL subagents use: dynamic | haiku | sonnet | tiered
	WorkerTools     []string `toml:"worker_tools"`      // tools the tiny workers (0.8B/2B) may use; ["all"] = no stripping
	SonnetTools     []string `toml:"sonnet_tools"`      // tools the 4B worker may use (research + Write); ["all"] = no stripping
	WorkerMaxTokens int      `toml:"worker_max_tokens"` // generation cap for the 0.8B/2B research tier (loop guard); 0 = uncapped
	SonnetMaxTokens int      `toml:"sonnet_max_tokens"` // generation cap for the 4B collation tier; 0 = uncapped
}

type HuggingFace struct {
	Token string `toml:"token"`
}

type Paths struct {
	ModelsDir string `toml:"models_dir"`
}

type CustomModel struct {
	Alias string `toml:"alias"`
	Repo  string `toml:"repo"`
	File  string `toml:"file"`
	Tier  string `toml:"tier"`
}

// defaultTOML is written verbatim on first run. Keep it in sync with the structs.
const defaultTOML = `# winc.toml - the one and only winc.cpp config. Edit freely; read on every run.

[general]
default_app   = "claude"        # claude | opencode | openclaw | cli
default_model = "qwen3.5-9b"  # alias from ` + "`winc ls`" + `
host = "127.0.0.1"
port = 8080

[reasoning]
# mode: adaptive | on | off | fixed   (default adaptive)
mode = "adaptive"
#   adaptive -> per-request thinking CEILING scaled to request size (snappy small, full for big)
#   on       -> model thinks freely, unrestricted (best quality, slowest first text)
#   off      -> never think (fastest)            [enable_thinking=false]
#   fixed    -> always fixed_budget_tokens
fixed_budget_tokens = 2048

[reasoning.adaptive]
# Budget is a CEILING (model may think less). First tier whose max_input_tokens >= request wins.
estimate = "chars"               # "chars" (fast ~4 ch/tok) | "count_tokens" (exact, +1 call)
tiers = [
  { max_input_tokens = 64,    budget_tokens = 0    },  # "Hey, how are you?"  -> instant
  { max_input_tokens = 512,   budget_tokens = 512  },
  { max_input_tokens = 4000,  budget_tokens = 2048 },
  { max_input_tokens = 16000, budget_tokens = 4096 },
]
ceiling_budget_tokens = 8192     # above the last tier
complexity_boost = true          # +1 tier if code / tool_result / build-intent verbs present

[performance]
backend    = "auto"     # auto | cuda | metal | vulkan | rocm | cpu
gpu_layers = "auto"     # "auto" or integer (-ngl)
context    = "auto"     # "auto" sizes the window to fit VRAM (falls back if too big), or a token count (-c)
batch      = "auto"
flash_attn = true
cache_type = "auto"     # KV cache: "auto" (q8_0; drops to q4_0 when the window would be starved) | q8_0 | f16 (max quality) | q4_0 (smallest -> widest auto context). Needs flash_attn.
threads    = "auto"
max_output_tokens = "auto"   # "auto" (~half the context) or an integer; caps the agent's response length

# MoE expert offload: keep a MoE model's expert weights in RAM (attention + MTP heads
# stay on GPU) so big MoE models run on smaller VRAM AND free VRAM for a much larger
# context. "on" is the lever for big context on a tight-fit MoE (e.g. a 35B on 16 GB).
cpu_moe = "auto"             # auto = offload when the model won't fit OR leaves no room for context; "on" forces it (big context, a bit slower); "off" disables; or a layer count

# Speculative decoding: a small same-family draft model predicts tokens the main
# model verifies in a batch (faster on dense models). Filename of a GGUF in models/.
draft_model = ""             # e.g. "Qwen3.5-0.8B-Q4_K_M.gguf"; blank = off

# Multi-Token Prediction (MTP): built-in speculative decoding baked into *-MTP model
# variants (e.g. qwen3.6-35b-mtp). Auto-enabled when an MTP GGUF is loaded AND the
# engine supports it; harmlessly skipped otherwise. ~1.4-2.2x on dense, ~1.2x on MoE.
mtp = "auto"                 # "auto" (on for MTP models) | "off"
mtp_draft_max = 2            # tokens drafted per step (--spec-draft-n-max); 2 is a good default

# Advanced escape hatch: extra llama-server flags appended verbatim.
extra_server_args = []       # e.g. ["--mlock"] or ["--prio", "2"] or ["--n-cpu-moe", "16"]

[multi]                  # llama-swap, only with ` + "`winc -s ... --multi`" + `
enabled = false
ttl_seconds = 600
sonnet = "qwen3.5-9b"   # Claude Code's claude-sonnet slot -> this local model alias
opus   = "qwen3.5-9b"
haiku  = "qwen3.5-9b"

[team]                     # agent hierarchy -- DEFAULT for a big main model
# The model you launch orchestrates as the MAIN agent while a small worker runs on the
# CPU (never touching the main model's VRAM) and handles ALL subagents -- so a
# deep-research fan-out spins up many quick small agents instead of clones of the big
# model. winc offers to download a missing worker and ships ready-made research/collator/
# code-reviewer agents. On by default for any model above the nano tier; the workers are fit
# to available RAM (largest dropped first; single only if not even the smallest fits). Pass
# --noteam for a single model.
mode      = "auto"          # auto (team for >nano-tier models w/ RAM to spare) | on (always) | off (never)
subagents = "dynamic"       # which worker subagents use (HEAD always stays on the big model):
                            #   dynamic -> start on the 0.8B and ESCALATE by request load through
                            #              mid (2B) -> 4B -> main model (only if VRAM allows)  [default]
                            #   haiku   -> always the 0.8B worker (cheapest, no escalation)
                            #   sonnet  -> always the 4B worker (more capable)
                            #   tiered  -> haiku + sonnet workers, per-agent pins, no auto-escalation
sonnet    = "qwen3.5-4b"    # the "sonnet" worker model (escalation target / sonnet tier)
mid       = "qwen3.5-2b"    # dynamic-mode middle rung between the 0.8B and 4B; "off" to disable
haiku     = "qwen3.5-0.8b"  # the "haiku" worker model (default subagent / research)
parallel  = 4               # concurrent slots on the haiku/mid workers (fan-out width).
                            # On small-RAM systems (<=16GB) winc halves the slots but keeps
                            # each worker's total window, doubling per-agent context.
# Per-tier tool allowlists: winc strips a worker request's tool set to its tier's list (the
# HEAD model always keeps every tool). Tiny workers stay research-only; the 4B also gets
# Write for collation/review. Use ["all"] to disable stripping for a tier.
worker_tools = ["WebSearch", "WebFetch", "Read", "Grep", "Glob"]           # 0.8B / 2B
sonnet_tools = ["WebSearch", "WebFetch", "Read", "Grep", "Glob", "Write"]  # 4B (collator/review)
# Worker generation caps (loop guard): a small model can otherwise run away and generate
# until it slams into its context window (minutes of CPU time for truncated garbage). winc
# lowers an over-large max_tokens to these ceilings for worker requests only (never the main
# model). Research outputs are short; collation gets more room. 0 = uncapped.
worker_max_tokens = 1536    # 0.8B / 2B research tier
sonnet_max_tokens = 4096    # 4B collation / review tier

[huggingface]
token = ""               # gated repos; or use the HF_TOKEN env var

[paths]
models_dir = ""          # blank = <install>/models

# Extend the built-in catalog with your own GGUFs:
# [[custom_models]]
# alias = "my-model"
# repo  = "user/Repo-GGUF"
# file  = "model-Q4_K_M.gguf"
# tier  = "nano"
`

// Defaults returns the parsed default configuration (kept in sync with defaultTOML).
func Defaults() Config {
	var c Config
	if err := toml.Unmarshal([]byte(defaultTOML), &c); err != nil {
		panic("winc: bad embedded default config: " + err.Error())
	}
	return c
}

// Load reads winc.toml, writing the default template first if it doesn't exist.
// Missing/empty critical fields are backfilled from defaults so a partial file
// never breaks winc.
func Load() (*Config, error) {
	p := paths.ConfigPath()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if werr := os.WriteFile(p, []byte(defaultTOML), 0o600); werr != nil {
			return nil, werr
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	cfg := Defaults()
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.backfill()
	return &cfg, nil
}

var (
	defaultModelLine = regexp.MustCompile(`(?m)^(\s*default_model\s*=\s*)"[^"]*"`)
	defaultAppLine   = regexp.MustCompile(`(?m)^(\s*default_app\s*=\s*)"[^"]*"`)
)

// UpdateDefaultModel rewrites the default_model value in winc.toml in place,
// preserving the rest of the file (and the user's other edits). No-op if the
// line isn't present.
func UpdateDefaultModel(alias string) error {
	return updateLine(defaultModelLine, alias)
}

// UpdateDefaultApp rewrites the default_app value in winc.toml in place,
// preserving the rest of the file. No-op if the line isn't present.
func UpdateDefaultApp(app string) error {
	return updateLine(defaultAppLine, app)
}

func updateLine(re *regexp.Regexp, val string) error {
	p := paths.ConfigPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !re.Match(data) {
		return nil
	}
	out := re.ReplaceAll(data, []byte(`${1}"`+val+`"`))
	return os.WriteFile(p, out, 0o600)
}

// EnsureExists writes the default template if winc.toml is missing. Returns true
// if it created the file. Idempotent.
func EnsureExists() (bool, error) {
	p := paths.ConfigPath()
	if _, err := os.Stat(p); err == nil {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(defaultTOML), 0o600)
}

func (c *Config) backfill() {
	d := Defaults()
	if c.General.DefaultApp == "" {
		c.General.DefaultApp = d.General.DefaultApp
	}
	if c.General.Host == "" {
		c.General.Host = d.General.Host
	}
	if c.General.Port == 0 {
		c.General.Port = d.General.Port
	}
	if c.Reasoning.Mode == "" {
		c.Reasoning.Mode = d.Reasoning.Mode
	}
	if c.Reasoning.FixedBudgetTokens == 0 {
		c.Reasoning.FixedBudgetTokens = d.Reasoning.FixedBudgetTokens
	}
	if c.Reasoning.Adaptive.Estimate == "" {
		c.Reasoning.Adaptive.Estimate = d.Reasoning.Adaptive.Estimate
	}
	if len(c.Reasoning.Adaptive.Tiers) == 0 {
		c.Reasoning.Adaptive.Tiers = d.Reasoning.Adaptive.Tiers
	}
	if c.Reasoning.Adaptive.CeilingBudgetTokens == 0 {
		c.Reasoning.Adaptive.CeilingBudgetTokens = d.Reasoning.Adaptive.CeilingBudgetTokens
	}
	if c.Performance.Backend == "" {
		c.Performance.Backend = d.Performance.Backend
	}
	if c.Performance.GpuLayers == "" {
		c.Performance.GpuLayers = d.Performance.GpuLayers
	}
	if c.Performance.Context == "" {
		c.Performance.Context = d.Performance.Context
	}
	if c.Performance.Batch == "" {
		c.Performance.Batch = d.Performance.Batch
	}
	if c.Performance.CacheType == "" {
		c.Performance.CacheType = d.Performance.CacheType
	}
	if c.Performance.Threads == "" {
		c.Performance.Threads = d.Performance.Threads
	}
	if c.Performance.MaxOutputTokens == "" {
		c.Performance.MaxOutputTokens = d.Performance.MaxOutputTokens
	}
	if c.Performance.CpuMoe == "" {
		c.Performance.CpuMoe = d.Performance.CpuMoe
	}
	if c.Performance.Mtp == "" {
		c.Performance.Mtp = d.Performance.Mtp
	}
	if c.Performance.MtpDraftMax == 0 {
		c.Performance.MtpDraftMax = d.Performance.MtpDraftMax
	}
	if c.Multi.TTLSeconds == 0 {
		c.Multi.TTLSeconds = d.Multi.TTLSeconds
	}
	if c.Team.Mode == "" {
		c.Team.Mode = d.Team.Mode
	}
	if c.Team.Subagents == "" {
		c.Team.Subagents = d.Team.Subagents
	}
	if c.Team.Sonnet == "" {
		c.Team.Sonnet = d.Team.Sonnet
	}
	if c.Team.Mid == "" {
		c.Team.Mid = d.Team.Mid
	}
	if c.Team.Haiku == "" {
		c.Team.Haiku = d.Team.Haiku
	}
	if c.Team.Parallel == 0 {
		c.Team.Parallel = d.Team.Parallel
	}
	if len(c.Team.WorkerTools) == 0 {
		c.Team.WorkerTools = d.Team.WorkerTools
	}
	if len(c.Team.SonnetTools) == 0 {
		c.Team.SonnetTools = d.Team.SonnetTools
	}
}
