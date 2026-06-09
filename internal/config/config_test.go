package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadWritesDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reasoning.Mode != "adaptive" {
		t.Fatalf("default reasoning mode = %q, want adaptive", cfg.Reasoning.Mode)
	}
	if cfg.General.Port != 8080 {
		t.Fatalf("default port = %d, want 8080", cfg.General.Port)
	}
	if len(cfg.Reasoning.Adaptive.Tiers) == 0 {
		t.Fatal("no adaptive tiers parsed")
	}
	if _, err := os.Stat(filepath.Join(dir, "winc.toml")); err != nil {
		t.Fatalf("winc.toml not written: %v", err)
	}
}

func TestUpdateDefaultModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if _, err := Load(); err != nil { // writes the default winc.toml
		t.Fatal(err)
	}
	if err := UpdateDefaultModel("qwen3.6-35b"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.DefaultModel != "qwen3.6-35b" {
		t.Fatalf("default_model = %q, want qwen3.6-35b", cfg.General.DefaultModel)
	}
}

func TestBackfill(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	// partial config: only sets one field
	if err := os.WriteFile(filepath.Join(dir, "winc.toml"), []byte("[general]\ndefault_model=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.Host == "" || cfg.General.Port == 0 || cfg.Reasoning.Mode == "" {
		t.Fatalf("backfill failed: %+v", cfg.General)
	}
}

func TestTeamDefaults(t *testing.T) {
	d := Defaults()
	if d.Team.Mode != "auto" {
		t.Fatalf("team mode default = %q, want auto (team is the default)", d.Team.Mode)
	}
	if d.Team.Subagents == "" || d.Team.Sonnet == "" || d.Team.Haiku == "" {
		t.Fatalf("team defaults missing: %+v", d.Team)
	}
	if d.Team.Parallel <= 0 {
		t.Fatalf("team parallel default = %d, want > 0", d.Team.Parallel)
	}
	// Backfill must fill team fields when a config omits [team] entirely -- so an existing
	// pre-team winc.toml still gets team-by-default (mode auto).
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "winc.toml"), []byte("[general]\ndefault_model=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Team.Mode != "auto" || cfg.Team.Subagents == "" || cfg.Team.Parallel == 0 {
		t.Fatalf("team backfill failed: %+v", cfg.Team)
	}
}
