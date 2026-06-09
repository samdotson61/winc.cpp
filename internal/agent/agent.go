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
// > 0, raises Claude Code's response-length cap. contextWindow, when > 0, tells
// Claude Code the local model's REAL context size so its auto-compaction fires
// before the server overflows (it otherwise assumes a ~200k cloud window).
// mainModel, when set, pins the TOP-LEVEL agent's model (ANTHROPIC_MODEL) -- needed
// in team mode, where the sonnet/haiku tiers point at small workers and the default
// main tier (sonnet) would otherwise demote the orchestrator to a worker model.
// subagentModel, when set, forces EVERY subagent (the Task tool AND the Workflow
// orchestrator's fan-out) onto that model -- so a deep-research fan-out uses quick small
// agents instead of clones of the big model.
func Env(baseURL string, slots Slots, maxOutputTokens, contextWindow int, mainModel, subagentModel string) []string {
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
	// Pin the main agent (else Claude Code defaults the top-level loop to the sonnet
	// tier -- which in team mode is a small worker, not the model the user launched).
	add("ANTHROPIC_MODEL", mainModel)
	// Force every subagent (Task tool + the Workflow orchestrator's fan-out) onto the
	// small worker. Highest precedence -- overrides per-agent model pins.
	add("CLAUDE_CODE_SUBAGENT_MODEL", subagentModel)
	add("CLAUDE_CONFIG_DIR", paths.ClaudeLocalDir())
	// 24-bit color + synchronized output (terminal mode 2026) glitch on terminals
	// that don't support them -- notably macOS Terminal.app. Only advertise them
	// elsewhere; capable terminals (iTerm2, Windows Terminal, etc.) set COLORTERM
	// themselves and Claude Code auto-detects synchronized output.
	if os.Getenv("TERM_PROGRAM") != "Apple_Terminal" {
		add("CLAUDE_FORCE_SYNCHRONIZED_OUTPUT", "1")
		add("COLORTERM", "truecolor")
	}
	if maxOutputTokens > 0 {
		add("CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(maxOutputTokens))
	}
	if contextWindow > 0 {
		// Size auto-compaction to the real local window, and trigger at 93% -- a ~7%
		// buffer for the in-flight response, so more of the window is usable while a
		// turn still rarely overruns the server's context.
		add("CLAUDE_CODE_AUTO_COMPACT_WINDOW", strconv.Itoa(contextWindow))
		add("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "93")
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
