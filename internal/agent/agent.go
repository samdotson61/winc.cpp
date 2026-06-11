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

// claudeMinWindow is the smallest CLAUDE_CODE_AUTO_COMPACT_WINDOW value Claude
// Code accepts (2.1.x validates the env var against [100000, 1000000]; an
// out-of-range value is treated as unset). Real windows below it are reported
// AS this floor, with the compaction trigger repositioned via the percentage
// override so it still fires at the real window's safe point.
const claudeMinWindow = 100000

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
		// Size auto-compaction to the real local window -- and trigger it EARLY enough
		// that the compaction request itself (the whole transcript plus the generated
		// summary) still fits. A fixed high trigger leaves only a sliver at local
		// window sizes: one big tool result jumps from just under the trigger straight
		// past the end of the window, and the recovery compaction then truncates
		// mid-summary -- a death loop the session never escapes (observed live at a
		// 49k window: overflow -> truncated summary -> overflow, every ~90s). Reserve
		// max(8k, window/8) tokens for the in-flight turn plus the summary.
		reserve := contextWindow / 8
		if reserve < 8192 {
			reserve = 8192
		}
		// Claude Code 2.1.x validates CLAUDE_CODE_AUTO_COMPACT_WINDOW against a
		// 100,000-token MINIMUM (verified against the 2.1.173 binary): a smaller
		// real window is rejected as invalid and silently replaced by the 100k
		// default -- the agent then believes it has room it doesn't (observed
		// live: a real 32k slot, the agent at "26%", generation truncating at
		// the wall for 20+ turns). So tell it a window it will ACCEPT, and place
		// the compaction trigger absolutely via the percentage override, which
		// is an unclamped parseFloat: pct of the believed window == the real
		// window minus the reserve. For real windows >= 100k this reduces to
		// exactly the old behavior.
		believed := contextWindow
		if believed < claudeMinWindow {
			believed = claudeMinWindow
		}
		pct := (contextWindow - reserve) * 100 / believed
		if pct < 10 {
			pct = 10
		} else if pct > 90 {
			pct = 90
		}
		add("CLAUDE_CODE_AUTO_COMPACT_WINDOW", strconv.Itoa(believed))
		add("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", strconv.Itoa(pct))
	}
	// Local models are far slower than the cloud -- a long prefill / time-to-first-token
	// on a low-end or CPU box trips Claude Code's stream-idle watchdog (~90s default) and
	// surfaces as "<model> is temporarily unavailable" (then a retry). Raise the overall
	// request and stream-idle timeouts so slow-but-valid responses complete. Only set a
	// value the user hasn't chosen themselves.
	setDefault := func(k, v string) {
		if os.Getenv(k) == "" {
			add(k, v)
		}
	}
	setDefault("API_TIMEOUT_MS", "1800000")               // 30 min overall request ceiling
	setDefault("CLAUDE_STREAM_IDLE_TIMEOUT_MS", "300000") // 5 min time-to-first-token / stall
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
