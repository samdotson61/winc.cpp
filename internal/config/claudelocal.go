package config

import (
	"os"
	"path/filepath"

	"winc/internal/paths"
)

// EnsureClaudeLocal creates the isolated Claude Code config dir for the local
// instance, so it never touches the user's logged-in cloud Claude Code. Returns
// the directory path. Idempotent.
func EnsureClaudeLocal() (string, error) {
	dir := paths.ClaudeLocalDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	settings := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(settings); os.IsNotExist(err) {
		_ = os.WriteFile(settings, []byte("{}\n"), 0o644)
	}
	return dir, nil
}
