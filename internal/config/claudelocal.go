package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"winc/internal/paths"
)

// preApprovedTools are pre-allowed in the sandbox settings.json so the local agent (and
// every subagent it spawns, including Workflow fan-out) never prompts to use them each
// launch -- web search/fetch plus read-only inspection. Write/Edit/Bash stay prompt-on-use;
// the user's own deny rules still take precedence over these allows.
var preApprovedTools = []string{"WebSearch", "WebFetch", "Read", "Grep", "Glob"}

// EnsureClaudeLocal creates the isolated Claude Code config dir for the local instance,
// so it never touches the user's logged-in cloud Claude Code, and ensures the pre-approved
// tools are allowed in its settings.json. Returns the directory path. Idempotent.
func EnsureClaudeLocal() (string, error) {
	dir := paths.ClaudeLocalDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ensureToolPermissions(filepath.Join(dir, "settings.json"))
	return dir, nil
}

// ensureToolPermissions merges preApprovedTools into settings.json's permissions.allow,
// preserving any existing content. Best-effort (a broken/missing file is replaced).
func ensureToolPermissions(path string) {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &root) // tolerate empty/invalid -> start fresh
	}
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	have := map[string]bool{}
	for _, v := range allow {
		if s, ok := v.(string); ok {
			have[s] = true
		}
	}
	for _, t := range preApprovedTools {
		if !have[t] {
			allow = append(allow, t)
			have[t] = true
		}
	}
	perms["allow"] = allow
	root["permissions"] = perms
	if out, err := json.MarshalIndent(root, "", "  "); err == nil {
		_ = os.WriteFile(path, append(out, '\n'), 0o644)
	}
}
