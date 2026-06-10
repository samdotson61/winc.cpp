package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestUpdateDefaultApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if _, err := Load(); err != nil { // writes the default winc.toml
		t.Fatal(err)
	}
	if err := UpdateDefaultApp("opencode"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.DefaultApp != "opencode" {
		t.Fatalf("default_app = %q, want opencode", cfg.General.DefaultApp)
	}
	// The model line must be untouched by an app update.
	if cfg.General.DefaultModel != Defaults().General.DefaultModel {
		t.Fatalf("default_model changed by an app update: %q", cfg.General.DefaultModel)
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
	if d.Team.Subagents == "" || d.Team.Sonnet == "" || d.Team.Haiku == "" || d.Team.Mid == "" {
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
	if cfg.Team.Mode != "auto" || cfg.Team.Subagents == "" || cfg.Team.Parallel == 0 || cfg.Team.Mid == "" {
		t.Fatalf("team backfill failed: %+v", cfg.Team)
	}
}

func TestEnsureClaudeLocalPreApprovesWebSearch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if _, err := EnsureClaudeLocal(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".claude-local", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	has := func(s string) bool {
		for _, a := range root.Permissions.Allow {
			if a == s {
				return true
			}
		}
		return false
	}
	if !has("WebSearch") || !has("WebFetch") {
		t.Fatalf("web tools not pre-approved (the every-launch headache): %v", root.Permissions.Allow)
	}
}

func TestEnsureClaudeLocalMergesExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	cl := filepath.Join(dir, ".claude-local")
	if err := os.MkdirAll(cl, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing custom allow + a deny rule must survive the merge.
	if err := os.WriteFile(filepath.Join(cl, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(npm test:*)"],"deny":["WebFetch"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureClaudeLocal(); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, filepath.Join(cl, "settings.json")))
	for _, want := range []string{"Bash(npm test:*)", "WebSearch", "WebFetch", `"deny"`} {
		if !strings.Contains(s, want) {
			t.Errorf("merge dropped %q: %s", want, s)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTeamToolAllowlists(t *testing.T) {
	hasWrite := func(ss []string) bool {
		for _, s := range ss {
			if s == "Write" {
				return true
			}
		}
		return false
	}
	d := Defaults()
	if len(d.Team.WorkerTools) == 0 || len(d.Team.SonnetTools) == 0 {
		t.Fatalf("tool allowlists missing from defaults: %+v", d.Team)
	}
	if hasWrite(d.Team.WorkerTools) {
		t.Error("tiny workers (worker_tools) must NOT include Write")
	}
	if !hasWrite(d.Team.SonnetTools) {
		t.Error("the 4B worker (sonnet_tools) should include Write")
	}
	// A config without [team] still gets the allowlists via backfill.
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "winc.toml"), []byte("[general]\ndefault_model=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Team.WorkerTools) == 0 || len(cfg.Team.SonnetTools) == 0 {
		t.Fatalf("tool allowlists not backfilled: %+v", cfg.Team)
	}
}

func TestSyncMissingSections(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	// A pre-[team] config: custom [general] + a partial [performance], no [team].
	old := "[general]\ndefault_model = \"my-model\"\nhost = \"10.0.0.1\"\n\n[performance]\nbackend = \"cuda\"\n"
	p := filepath.Join(dir, "winc.toml")
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := SyncMissingSections()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range added {
		got[a] = true
	}
	if !got["team"] {
		t.Errorf("expected [team] to be appended, got %v", added)
	}

	out := string(mustRead(t, p))
	// Existing user content is preserved verbatim (append-only).
	if !strings.Contains(out, `default_model = "my-model"`) || !strings.Contains(out, `host = "10.0.0.1"`) || !strings.Contains(out, `backend = "cuda"`) {
		t.Errorf("existing content not preserved:\n%s", out)
	}
	// The new section is now present and tunable.
	if !strings.Contains(out, "[team]") || !strings.Contains(out, "subagents") {
		t.Errorf("[team] not appended:\n%s", out)
	}
	// Idempotent: a second sync adds nothing.
	if again, err := SyncMissingSections(); err != nil || len(again) != 0 {
		t.Errorf("second sync should be a no-op, got %v (err %v)", again, err)
	}
}
