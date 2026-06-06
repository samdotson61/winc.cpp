// Package agent builds the launch environment for a coding agent (Claude Code,
// OpenCode, OpenClaw) and runs it pointed at the local endpoint, isolated from
// the user's cloud Claude Code.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"

	"winc/internal/paths"
)

// Slots holds the model names mapped onto Claude Code's claude-* tiers.
type Slots struct{ Sonnet, Opus, Haiku string }

// Env returns the full environment for the agent process. maxOutputTokens, when
// > 0, raises Claude Code's response-length cap so big agentic edits don't hit
// the default 32000-token limit.
func Env(baseURL string, slots Slots, maxOutputTokens int) []string {
	env := os.Environ()
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("ANTHROPIC_BASE_URL", baseURL)
	// Only AUTH_TOKEN (ignored locally). Setting ANTHROPIC_API_KEY too makes
	// Claude Code warn "both set; auth may not work".
	add("ANTHROPIC_AUTH_TOKEN", "winc-local")
	add("ANTHROPIC_DEFAULT_SONNET_MODEL", slots.Sonnet)
	add("ANTHROPIC_DEFAULT_OPUS_MODEL", slots.Opus)
	add("ANTHROPIC_DEFAULT_HAIKU_MODEL", slots.Haiku)
	add("CLAUDE_CONFIG_DIR", paths.ClaudeLocalDir())
	add("CLAUDE_FORCE_SYNCHRONIZED_OUTPUT", "1")
	add("COLORTERM", "truecolor")
	if maxOutputTokens > 0 {
		add("CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(maxOutputTokens))
	}
	return env
}

func command(app string) (name string, args []string, ok bool) {
	switch app {
	case "claude":
		return "claude", nil, true
	case "opencode":
		return "opencode", nil, true
	case "openclaw":
		return "openclaw", []string{"tui"}, true
	}
	return "", nil, false
}

// Available reports whether the agent's launcher is on PATH.
func Available(app string) bool {
	name, _, ok := command(app)
	if !ok {
		return false
	}
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	// Windows npm shims (claude.cmd) are found by LookPath via PATHEXT; if not,
	// cmd /c may still resolve them, so don't hard-fail on Windows.
	return runtime.GOOS == "windows"
}

// Launch runs the agent interactively (inherits stdio), blocking until it exits.
func Launch(app string, env []string) error {
	name, args, ok := command(app)
	if !ok {
		return fmt.Errorf("unknown app %q (use claude, opencode, or openclaw)", app)
	}
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", append([]string{"/c", name}, args...)...)
	} else {
		c = exec.Command(name, args...)
	}
	c.Env = env
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
