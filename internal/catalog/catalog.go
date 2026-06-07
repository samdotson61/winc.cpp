// Package catalog is the shipped model catalogue (embedded JSON), merged with any
// user-defined [[custom_models]] from winc.toml. It is the single source of truth
// for model aliases, HuggingFace repos/files, and memory tiers.
package catalog

import (
	_ "embed"
	"encoding/json"
	"strings"

	"winc/internal/config"
)

//go:embed catalog.json
var catalogJSON []byte

type Model struct {
	Tier  string `json:"tier"`
	Alias string `json:"alias"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Repo  string `json:"repo"`
	File  string `json:"file"`
	Draft string `json:"draft"` // alias of a same-tokenizer draft model (speculative decoding); "" = none
	Note  string `json:"note"`
}

type Catalog struct {
	Tiers  map[string]string `json:"tiers"`
	Models []Model           `json:"models"`
}

// TierOrder is smallest -> largest, used for display and selection.
var TierOrder = []string{"nano", "small", "mid", "large", "xl", "custom"}

// Load parses the embedded catalogue and appends user custom models.
func Load(custom []config.CustomModel) *Catalog {
	var c Catalog
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic("winc: bad embedded catalog.json: " + err.Error())
	}
	if c.Tiers == nil {
		c.Tiers = map[string]string{}
	}
	for _, cm := range custom {
		if cm.Alias == "" || cm.Repo == "" || cm.File == "" {
			continue
		}
		tier := cm.Tier
		if tier == "" {
			tier = "custom"
		}
		if _, ok := c.Tiers["custom"]; !ok {
			c.Tiers["custom"] = "user-defined (winc.toml)"
		}
		c.Models = append(c.Models, Model{
			Tier: tier, Alias: cm.Alias, Name: cm.Alias, Size: "?",
			Repo: cm.Repo, File: cm.File, Note: "custom (winc.toml)",
		})
	}
	return &c
}

// Find resolves a query against alias / file / name (case-insensitive).
func (c *Catalog) Find(q string) *Model {
	if q == "" {
		return nil
	}
	for i := range c.Models {
		m := &c.Models[i]
		if strings.EqualFold(m.Alias, q) || strings.EqualFold(m.File, q) || strings.EqualFold(m.Name, q) {
			return m
		}
	}
	return nil
}

// DraftFor returns the catalogued draft model paired with a (dense) target, or nil.
// MoE targets carry no draft mapping, so they are never paired -- speculative
// decoding is net-negative on MoE (only ~3B active, nothing for a draft to save).
func (c *Catalog) DraftFor(m *Model) *Model {
	if m == nil || m.Draft == "" {
		return nil
	}
	return c.Find(m.Draft)
}

// ByTier returns the models in a given tier (catalogue order = best first).
func (c *Catalog) ByTier(tier string) []Model {
	var out []Model
	for _, m := range c.Models {
		if m.Tier == tier {
			out = append(out, m)
		}
	}
	return out
}

// VramTier maps a memory budget in MB (discrete VRAM or Apple unified) to a tier.
func VramTier(mb int) string {
	switch {
	case mb >= 48000:
		return "xl"
	case mb >= 22000:
		return "large"
	case mb >= 12000:
		return "mid"
	case mb >= 6000:
		return "small"
	default:
		return "nano"
	}
}
