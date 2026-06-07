// Package catalog is the shipped model catalogue (embedded JSON), merged with any
// user-defined [[custom_models]] from winc.toml. It is the single source of truth
// for model aliases, HuggingFace repos/files, and memory tiers.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"winc/internal/config"
	"winc/internal/paths"
)

//go:embed catalog.json
var catalogJSON []byte

// sourceURL is the canonical catalogue in the winc.cpp repo, fetched by `winc update`.
const sourceURL = "https://raw.githubusercontent.com/samdotson61/winc.cpp/master/internal/catalog/catalog.json"

type Model struct {
	Tier  string `json:"tier"`
	Alias string `json:"alias"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Repo  string `json:"repo"`
	File  string `json:"file"`           // filename in the HF repo to download
	Save  string `json:"save,omitempty"` // local filename to save as (default: File); used to disambiguate MTP variants
	Draft string `json:"draft,omitempty"` // alias of a same-tokenizer draft model (speculative decoding); "" = none
	Mtp   string `json:"mtp,omitempty"`   // alias of this model's Multi-Token-Prediction variant; "" = none
	Note  string `json:"note"`
}

// LocalFile is the on-disk filename winc saves/looks for (Save if set, else File).
func (m *Model) LocalFile() string {
	if m == nil {
		return ""
	}
	if m.Save != "" {
		return m.Save
	}
	return m.File
}

type Catalog struct {
	Tiers  map[string]string `json:"tiers"`
	Models []Model           `json:"models"`
}

// TierOrder is smallest -> largest, used for display and selection. "mtp" holds
// faster Multi-Token-Prediction model variants (shown last, never auto-recommended).
var TierOrder = []string{"nano", "small", "mid", "large", "xl", "mtp", "custom"}

// parseCatalog unmarshals + sanity-checks catalogue JSON. ok=false on malformed or
// implausibly small payloads (so a truncated download never replaces a good catalogue).
func parseCatalog(data []byte) (*Catalog, bool) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	if len(c.Models) < 5 {
		return nil, false
	}
	return &c, true
}

// loadBase returns the on-disk catalogue (written by `winc update`, or hand-placed to
// override) when present, valid, AND newer than this binary; otherwise the embedded
// catalogue. The freshness check matters on a git clone: a `winc update` rebuild bakes
// the latest catalogue into the binary, so a cache fetched earlier must not shadow it.
// On a prebuilt install the cache is always newer than the shipped binary, so it wins.
func loadBase() *Catalog {
	if data, err := os.ReadFile(paths.CatalogPath()); err == nil && cacheNewerThanBinary() {
		if c, ok := parseCatalog(data); ok {
			return c
		}
	}
	c, ok := parseCatalog(catalogJSON)
	if !ok {
		panic("winc: bad embedded catalog.json")
	}
	return c
}

// Source reports which catalogue is active: "updated cache" (a fresh on-disk override
// is in use) or "built-in" (the one embedded in this binary). Mirrors loadBase.
func Source() string {
	if data, err := os.ReadFile(paths.CatalogPath()); err == nil && cacheNewerThanBinary() {
		if _, ok := parseCatalog(data); ok {
			return "updated cache"
		}
	}
	return "built-in"
}

// cacheNewerThanBinary reports whether the on-disk catalog cache was written after this
// binary was built/installed. If the comparison can't be made, the cache is trusted.
func cacheNewerThanBinary() bool {
	ci, err := os.Stat(paths.CatalogPath())
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return true
	}
	ei, err := os.Stat(exe)
	if err != nil {
		return true
	}
	return ci.ModTime().After(ei.ModTime())
}

// Update fetches the latest catalogue from the winc.cpp repo, validates it, and caches
// it to disk so prebuilt-binary users get new models without rebuilding. Returns the
// new total model count. The embedded catalogue remains the offline fallback.
func Update() (int, error) {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Get(sourceURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d fetching catalog", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return 0, err
	}
	c, ok := parseCatalog(data)
	if !ok {
		return 0, fmt.Errorf("fetched catalog is malformed or truncated")
	}
	if err := os.WriteFile(paths.CatalogPath(), data, 0o644); err != nil {
		return 0, err
	}
	return len(c.Models), nil
}

// Load returns the active catalogue (on-disk override or embedded) plus user custom models.
func Load(custom []config.CustomModel) *Catalog {
	c := loadBase()
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
	return c
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
