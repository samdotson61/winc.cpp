package cli

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"winc/internal/agent"
	"winc/internal/platform"
	"winc/internal/ui"
)

// cmdInstall installs a coding agent so the user never has to leave winc to
// get one. Usage: winc install [claude] [stable|latest|<version>]. Claude Code
// is the only agent winc installs itself (its releases are published as
// plain, checksummed binaries); OpenCode / OpenClaw get a pointer.
func cmdInstall(args []string) int {
	app := "claude"
	target := ""
	for _, a := range args {
		switch l := strings.ToLower(a); {
		case l == "claude" || l == "claude-code":
			app = "claude"
		case l == "opencode" || l == "openclaw":
			app = l
		case agent.ValidInstallTarget(l):
			target = l
		default:
			ui.Err("unknown argument %q (usage: winc install [claude] [stable|latest|<version>])", a)
			return 1
		}
	}
	if app != "claude" {
		ui.Err("winc installs Claude Code only; install %s from its own site, then re-run.", app)
		return 1
	}
	if p, ok := agent.Resolve("claude"); ok && target == "" {
		ui.Good("Claude Code is already installed: %s", p)
		ui.Say("  update it with:  claude update")
		ui.Say("  switch channel/version with:  winc install claude stable|latest|<version>")
		return 0
	}
	return installClaude(target)
}

// installClaude runs the native install and, if that path fails, offers the
// platform's package manager before printing the manual commands. Returns an
// exit code.
func installClaude(target string) int {
	ui.Info("installing Claude Code (official release, sha256-verified) ...")
	err := agent.InstallClaude(target)
	if err == nil {
		p, _ := agent.Resolve("claude")
		ui.Good("Claude Code installed: %s", p)
		ensureClaudeOnPath(filepath.Dir(p))
		ui.Say("  'winc -s claude <model>' finds it right away; open a NEW terminal for a bare 'claude'.")
		return 0
	}
	ui.Warn("native install failed: %v", err)
	if tryPackageManagerClaude() {
		if p, ok := agent.Resolve("claude"); ok {
			ui.Good("Claude Code installed: %s", p)
			return 0
		}
	}
	printClaudeInstallHints()
	return 1
}

// ensureClaudeOnPath finishes what `claude install` leaves to the user: it
// places ~/.local/bin/claude but (observed live, 2.1.292) only prints "not in
// your PATH - add it ..." when that directory isn't on PATH. winc already
// records its own folder for every shell with a proven, idempotent helper
// (Windows user PATH; bash/zsh/profile + fish conf.d on Unix), so the same
// one records Claude Code's directory and a bare `claude` works in the next
// terminal. Skipped when the directory is already live or recorded -- on most
// Linux distros and on macOS with a stock profile ~/.local/bin already is.
func ensureClaudeOnPath(dir string) {
	if liveOnPath(dir) || platform.OnPath(dir) {
		return
	}
	if err := platform.AddDirToPath(dir); err != nil {
		ui.Warn("could not add %s to your PATH: %v", dir, err)
		return
	}
	if runtime.GOOS == "windows" {
		ui.Good("added %s to your user PATH", dir)
	} else {
		ui.Good("added %s to PATH (bash/zsh/profile, fish)", dir)
	}
}

// tryPackageManagerClaude offers the platform's package-manager install of
// Claude Code (winget on Windows, Homebrew on macOS) and runs it on a yes.
// Linux package repos need sudo + a key import, so they stay a printed hint.
func tryPackageManagerClaude() bool {
	if !ui.Interactive() {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		if platform.PkgManager() == "" {
			return false
		}
		if !ui.Confirm("Install Claude Code with winget instead?", true) {
			return false
		}
		if err := platform.InstallPackage("Anthropic.ClaudeCode"); err != nil {
			ui.Warn("winget install failed: %v", err)
			return false
		}
		return true
	case "darwin":
		if platform.PkgManager() == "" {
			return false
		}
		if !ui.Confirm("Install Claude Code with Homebrew instead?", true) {
			return false
		}
		if err := execInherit("brew", "install", "--cask", "claude-code").Run(); err != nil {
			ui.Warn("brew install failed: %v", err)
			return false
		}
		return true
	}
	return false
}

// printClaudeInstallHints shows the official install commands for this OS.
func printClaudeInstallHints() {
	ui.Say("  install Claude Code manually, then re-run:")
	switch runtime.GOOS {
	case "windows":
		ui.Say("    PowerShell:  irm https://claude.ai/install.ps1 | iex")
		ui.Say("    or:          winget install Anthropic.ClaudeCode")
	case "darwin":
		ui.Say("    curl -fsSL https://claude.ai/install.sh | bash")
		ui.Say("    or:  brew install --cask claude-code")
	default:
		ui.Say("    curl -fsSL https://claude.ai/install.sh | bash")
	}
	ui.Say("    docs: https://code.claude.com/docs/en/setup")
}

// ensureAgent gates a launch on the agent actually being present. A missing
// Claude Code is offered for install on the spot (interactive terminals only);
// a declined or failed install, or a missing OpenCode/OpenClaw, prints how to
// get it and returns false so the caller stops BEFORE loading a model.
func ensureAgent(app string) bool {
	if _, ok := agent.Resolve(app); ok {
		return true
	}
	if app != "claude" {
		ui.Err("%s not found on PATH - install it, then re-run.", app)
		return false
	}
	ui.Warn("Claude Code is not installed.")
	if ui.Interactive() && ui.Confirm("Install it now (official release, sha256-verified, ~250 MB)?", true) {
		return installClaude("") == 0
	}
	ui.Say("  install it any time with:  winc install claude")
	return false
}

// claudeVersion returns `claude --version`'s first line, or "".
func claudeVersion(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}
