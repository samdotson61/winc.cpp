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
	if _, err := EnsureClaudeLocal(true); err != nil {
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
	if !has("mcp__winc__web_search") || !has("WebFetch") {
		t.Fatalf("web tools not pre-approved (the every-launch headache): %v", root.Permissions.Allow)
	}
	if has("WebSearch") {
		t.Fatalf("the dead built-in WebSearch must not be pre-approved: %v", root.Permissions.Allow)
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
	if _, err := EnsureClaudeLocal(true); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, filepath.Join(cl, "settings.json")))
	for _, want := range []string{"Bash(npm test:*)", "mcp__winc__web_search", "WebFetch", `"deny"`} {
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

func TestJournalDefaults(t *testing.T) {
	d := Defaults()
	j := d.Journal
	if j.BudgetTokens != "auto" || j.RecallTokens != 400 || j.RecallTopK != 4 || j.SummaryTokens != 300 {
		t.Fatalf("journal defaults drifted: %+v", j)
	}
	// Default-on was Sam's call, gated on the journalbench results (2026-07-15:
	// recall 5/5 at d=40 vs trim-only 0/5); threshold 2.0 measured as the
	// lowest sweep value with zero filler-turn noise and full recall.
	if !j.Enabled || j.RecallThreshold != 2.0 {
		t.Fatalf("shipped defaults: enabled=true threshold=2.0, got %+v", j)
	}
	// A partial user file backfills the budget mode but respects an explicit
	// enabled= choice (bools override the default by unmarshal-into-defaults).
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	toml := "[journal]\nenabled = " + boolLit(!d.Journal.Enabled) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "winc.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Journal.Enabled == d.Journal.Enabled {
		t.Fatal("explicit enabled= must override the default")
	}
	if cfg.Journal.BudgetTokens != "auto" {
		t.Fatalf("budget mode not backfilled: %q", cfg.Journal.BudgetTokens)
	}
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
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
	if !got["journal"] {
		t.Errorf("expected [journal] to be appended, got %v", added)
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

// WriteAgentNotes provisions the sandboxed session's CLAUDE.md with the REAL
// window + measured speeds; it rewrites only winc-owned files (marker first
// line) and never clobbers user edits.
func TestWriteAgentNotesMarkerPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	p := filepath.Join(dir, ".claude-local", "CLAUDE.md")

	if err := WriteAgentNotes(65536, 65536, 38, 877, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{wincMarker, "65536", "~38 tok/s", "trimmed-context.md"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("notes missing %q:\n%s", want, data)
		}
	}

	// winc-owned -> rewritten with the new launch's numbers; unmeasured speeds omitted.
	if err := WriteAgentNotes(65536, 32768, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if !strings.Contains(string(data), "32768") || strings.Contains(string(data), "tok/s") {
		t.Errorf("winc-owned notes must be rewritten (per-slot window, no speeds when unmeasured):\n%s", data)
	}

	// User-owned -> never touched.
	if err := os.WriteFile(p, []byte("my own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentNotes(99999, 99999, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); string(data) != "my own notes\n" {
		t.Errorf("user-owned CLAUDE.md must never be clobbered, got:\n%s", data)
	}
}

// Existing sandboxes (pre-1.43) had the dead built-in WebSearch pre-approved. With
// search on it must move allow -> deny, the local tool must be allowed, the user's
// own rules must survive, and a second pass must change nothing.
func TestEnsureClaudeLocalMigratesDeadWebSearch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	cl := filepath.Join(dir, ".claude-local")
	if err := os.MkdirAll(cl, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cl, "settings.json")
	if err := os.WriteFile(p, []byte(`{"permissions":{"allow":["WebSearch","WebFetch","Read","Grep","Glob","Bash(go test:*)"],"deny":["Bash(rm -rf:*)"]},"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureClaudeLocal(true); err != nil {
		t.Fatal(err)
	}
	var root struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal(mustRead(t, p), &root); err != nil {
		t.Fatal(err)
	}
	contains := func(ss []string, s string) bool {
		for _, x := range ss {
			if x == s {
				return true
			}
		}
		return false
	}
	if contains(root.Permissions.Allow, "WebSearch") {
		t.Errorf("WebSearch still allowed: %v", root.Permissions.Allow)
	}
	if !contains(root.Permissions.Deny, "WebSearch") {
		t.Errorf("WebSearch not denied: %v", root.Permissions.Deny)
	}
	if !contains(root.Permissions.Allow, "mcp__winc__web_search") || !contains(root.Permissions.Allow, "Bash(go test:*)") || !contains(root.Permissions.Deny, "Bash(rm -rf:*)") || root.Theme != "dark" {
		t.Errorf("user rules / local tool lost: %+v", root)
	}
	first := string(mustRead(t, p))
	if _, err := EnsureClaudeLocal(true); err != nil {
		t.Fatal(err)
	}
	if again := string(mustRead(t, p)); again != first {
		t.Errorf("second pass changed the file:\n%s\n---\n%s", first, again)
	}
	if strings.Count(first, `"WebSearch"`) != 1 {
		t.Errorf("WebSearch must appear exactly once (in deny): %s", first)
	}
}

// With search OFF the permissions are left as they are: no local tool, no deny.
func TestEnsureClaudeLocalSearchOffLeavesRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if _, err := EnsureClaudeLocal(false); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, filepath.Join(dir, ".claude-local", "settings.json")))
	if strings.Contains(s, "mcp__winc__web_search") || strings.Contains(s, `"deny"`) {
		t.Errorf("search off must not register or deny anything: %s", s)
	}
	if !strings.Contains(s, "WebFetch") {
		t.Errorf("the other pre-approvals must still land: %s", s)
	}
}

// WriteMCPConfig points at the given binary with the mcp-search arg, is
// regenerated (a moved folder gets the new path) and is byte-stable otherwise.
func TestWriteMCPConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	p, err := WriteMCPConfig(`C:\old\winc.exe`)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, p), &doc); err != nil {
		t.Fatal(err)
	}
	w := doc.Servers["winc"]
	if w.Type != "stdio" || w.Command != `C:\old\winc.exe` || len(w.Args) != 1 || w.Args[0] != "mcp-search" {
		t.Fatalf("bad registration: %+v", doc)
	}
	first := mustRead(t, p)
	if _, err := WriteMCPConfig(`C:\old\winc.exe`); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, p)) != string(first) {
		t.Error("unchanged binary must produce an identical file")
	}
	if _, err := WriteMCPConfig(`D:\moved\winc.exe`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, p)), `D:\\moved\\winc.exe`) {
		t.Errorf("moved binary not re-registered: %s", mustRead(t, p))
	}
}

// Old winc.toml tool lists name WebSearch only: the file migration inserts the
// local tool after it (idempotent, nothing else touched) and Load carries it in
// memory even before the file is migrated.
func TestMigrateToolListsAndBackfill(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	old := "[general]\ndefault_model = \"m\"   # keep\n\n[team]\nmode = \"on\"\nworker_tools = [\"WebSearch\", \"WebFetch\", \"Read\"]   # 0.8B\nsonnet_tools = [\"all\"]\n"
	p := filepath.Join(dir, "winc.toml")
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Team.WorkerTools, ","); got != "WebSearch,mcp__winc__web_search,WebFetch,Read" {
		t.Errorf("backfill did not carry the local tool: %s", got)
	}
	if got := strings.Join(cfg.Team.SonnetTools, ","); got != "all" {
		t.Errorf("[\"all\"] must be untouched: %s", got)
	}
	if cfg.Search.Provider != "auto" || cfg.Search.MaxResults != 5 {
		t.Errorf("search defaults not backfilled: %+v", cfg.Search)
	}
	changed, err := MigrateToolLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "worker_tools" {
		t.Errorf("changed = %v, want [worker_tools]", changed)
	}
	out := string(mustRead(t, p))
	if !strings.Contains(out, "worker_tools = [\"WebSearch\", \"mcp__winc__web_search\", \"WebFetch\", \"Read\"]   # 0.8B") {
		t.Errorf("migration wrong:\n%s", out)
	}
	if !strings.Contains(out, "default_model = \"m\"   # keep") || !strings.Contains(out, "sonnet_tools = [\"all\"]") {
		t.Errorf("other lines disturbed:\n%s", out)
	}
	if again, err := MigrateToolLists(); err != nil || len(again) != 0 {
		t.Errorf("second migration must be a no-op: %v %v", again, err)
	}
	if string(mustRead(t, p)) != out {
		t.Error("second migration changed bytes")
	}
	// The new default template never needs migrating, and the [search] section arrives via sync.
	if err := os.WriteFile(p, []byte(defaultTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, _ := MigrateToolLists(); len(ch) != 0 {
		t.Errorf("default template should not migrate: %v", ch)
	}
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := SyncMissingSections()
	if err != nil {
		t.Fatal(err)
	}
	has := false
	for _, a := range added {
		if a == "search" {
			has = true
		}
	}
	if !has {
		t.Errorf("[search] not appended: %v", added)
	}
}

// The agent notes tell the model which search tool works here.
func TestWriteAgentNotesSearchLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	if err := WriteAgentNotes(32768, 32768, 0, 0, "mcp__winc__web_search"); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, filepath.Join(dir, ".claude-local", "CLAUDE.md")))
	if !strings.Contains(s, "mcp__winc__web_search") || !strings.Contains(s, "WebSearch tool is UNAVAILABLE") {
		t.Errorf("search note missing:\n%s", s)
	}
	if err := WriteAgentNotes(32768, 32768, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if s := string(mustRead(t, filepath.Join(dir, ".claude-local", "CLAUDE.md"))); strings.Contains(s, "mcp__winc__web_search") {
		t.Errorf("search off must not advertise the tool:\n%s", s)
	}
}

// WriteOpenCodeConfig registers winc as OpenCode's local MCP server and points
// its anthropic provider at winc; regenerated per launch, byte-stable otherwise.
func TestWriteOpenCodeConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINC_HOME", dir)
	p, err := WriteOpenCodeConfig(`C:\\w\\winc.exe`, "http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(mustRead(t, p), &doc); err != nil {
		t.Fatal(err)
	}
	w := doc.MCP["winc"]
	if w.Type != "local" || !w.Enabled || len(w.Command) != 2 || w.Command[0] != `C:\\w\\winc.exe` || w.Command[1] != "mcp-search" {
		t.Fatalf("bad mcp entry: %+v", doc.MCP)
	}
	if doc.Provider["anthropic"].Options["baseURL"] != "http://127.0.0.1:9" {
		t.Errorf("provider baseURL missing: %+v", doc.Provider)
	}
	first := mustRead(t, p)
	if _, err := WriteOpenCodeConfig(`C:\\w\\winc.exe`, "http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, p)) != string(first) {
		t.Error("unchanged inputs must produce an identical file")
	}
}
