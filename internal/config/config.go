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
	Backend   string `toml:"backend"`    // auto | cuda | metal | vulkan | rocm | cpu
	GpuLayers string `toml:"gpu_layers"` // "auto" or integer
	Context   string `toml:"context"`    // "auto" or integer
	Batch     string `toml:"batch"`      // "auto" or integer
	FlashAttn bool   `toml:"flash_attn"`
	CacheType string `toml:"cache_type"`        // e.g. q8_0, f16
	Threads   string `toml:"threads"`           // "auto" or integer
	MaxOutputTokens string `toml:"max_output_tokens"` // "auto" (~half context) or integer
}

type Multi struct {
	Enabled    bool   `toml:"enabled"`
	TTLSeconds int    `toml:"ttl_seconds"`
	Sonnet     string `toml:"sonnet"`
	Opus       string `toml:"opus"`
	Haiku      string `toml:"haiku"`
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
default_model = "qwen2.5-coder-7b"  # alias from ` + "`winc ls`" + `
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
cache_type = "q8_0"
threads    = "auto"
max_output_tokens = "auto"   # "auto" (~half the context) or an integer; caps the agent's response length

[multi]                  # llama-swap, only with ` + "`winc -s ... --multi`" + `
enabled = false
ttl_seconds = 600
sonnet = "qwen2.5-coder-7b"   # Claude Code's claude-sonnet slot -> this local model alias
opus   = "qwen2.5-coder-7b"
haiku  = "qwen2.5-coder-7b"

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
		if werr := os.WriteFile(p, []byte(defaultTOML), 0o644); werr != nil {
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

var defaultModelLine = regexp.MustCompile(`(?m)^(\s*default_model\s*=\s*)"[^"]*"`)

// UpdateDefaultModel rewrites the default_model value in winc.toml in place,
// preserving the rest of the file (and the user's other edits). No-op if the
// line isn't present.
func UpdateDefaultModel(alias string) error {
	p := paths.ConfigPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !defaultModelLine.Match(data) {
		return nil
	}
	out := defaultModelLine.ReplaceAll(data, []byte(`${1}"`+alias+`"`))
	return os.WriteFile(p, out, 0o644)
}

// EnsureExists writes the default template if winc.toml is missing. Returns true
// if it created the file. Idempotent.
func EnsureExists() (bool, error) {
	p := paths.ConfigPath()
	if _, err := os.Stat(p); err == nil {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(defaultTOML), 0o644)
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
	if c.Multi.TTLSeconds == 0 {
		c.Multi.TTLSeconds = d.Multi.TTLSeconds
	}
}
