package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"winc/internal/agent"
	"winc/internal/ui"
)

// OpenClaw integration. OpenClaw reads its model providers and MCP servers from
// its own config (openclaw.json) and nothing else -- the ANTHROPIC_* environment
// winc exports for Claude Code means nothing to it, so until v1.44 `winc -s
// openclaw <model>` launched a TUI that still talked to whatever provider the
// user had configured. winc now writes, through OpenClaw's own CLI (the only
// documented writer), three things:
//   - models.providers.winc: an anthropic-messages provider at winc's STABLE
//     endpoint (the router is pinned to the winc.toml port for this app so the
//     entry survives between launches), listing the launched model with its
//     real context window and output cap;
//   - agents.defaults.model.primary = "winc/<alias>" for the duration of the
//     session, restored to the previous value when the agent exits;
//   - mcp.servers.winc (see registerSearch): OpenClaw exposes the tool as
//     winc__web_search beside its own built-in web_search.
// Every write is check-then-act against `openclaw config get`, so an unchanged
// launch rewrites nothing.

const openClawProvider = "winc"

// openClawExec runs the openclaw CLI; a var so tests substitute a recorder.
var openClawExec = openClawRun

// openClawModelSpec is the models[] entry winc publishes for the launched model.
type openClawModelSpec struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
}

// openClawProviderSpec is models.providers.winc.
type openClawProviderSpec struct {
	BaseURL string              `json:"baseUrl"`
	APIKey  string              `json:"apiKey"`
	API     string              `json:"api"`
	Models  []openClawModelSpec `json:"models"`
}

// openClawProviderJSON builds the provider entry for one launch.
func openClawProviderJSON(baseURL, alias string, ctxWindow, maxOut int) string {
	spec := openClawProviderSpec{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  "winc-local",
		API:     "anthropic-messages",
		Models: []openClawModelSpec{{
			ID: alias, Name: alias, Reasoning: false, Input: []string{"text"},
			ContextWindow: ctxWindow, MaxTokens: maxOut,
		}},
	}
	b, _ := json.Marshal(spec)
	return string(b)
}

// openClawProviderMatches reports whether the stored provider (as `config get`
// prints it: JSON, apiKey redacted, extra defaulted fields) already describes
// the same endpoint and model as want.
func openClawProviderMatches(shown, want string) bool {
	i, j := strings.Index(shown, "{"), strings.LastIndex(shown, "}")
	if i < 0 || j <= i {
		return false
	}
	var got, exp openClawProviderSpec
	if json.Unmarshal([]byte(shown[i:j+1]), &got) != nil || json.Unmarshal([]byte(want), &exp) != nil {
		return false
	}
	if strings.TrimRight(got.BaseURL, "/") != exp.BaseURL || got.API != exp.API || len(got.Models) == 0 {
		return false
	}
	for _, m := range got.Models {
		if m.ID == exp.Models[0].ID {
			return m.ContextWindow == exp.Models[0].ContextWindow && m.MaxTokens == exp.Models[0].MaxTokens
		}
	}
	return false
}

// registerOpenClawModel points OpenClaw at the launched model: upserts the winc
// provider and makes "winc/<alias>" the default model for this session. The
// returned restore puts the previous default back (idempotent; safe to call
// twice, from both the normal exit and the Ctrl-C path). A missing openclaw or
// a failed write is reported and leaves a no-op restore.
func registerOpenClawModel(baseURL, alias string, ctxWindow, maxOut int) (restore func()) {
	restore = func() {}
	bin, ok := agent.Resolve("openclaw")
	if !ok {
		return restore
	}
	want := openClawProviderJSON(baseURL, alias, ctxWindow, maxOut)
	if cur, err := openClawExec(bin, "config", "get", "models.providers."+openClawProvider); err != nil || !openClawProviderMatches(cur, want) {
		if _, err := openClawExec(bin, "config", "set", "models.providers."+openClawProvider, "--strict-json", want); err != nil {
			ui.Warn("OpenClaw: could not write the winc provider: %v", err)
			return restore
		}
		ui.Info("OpenClaw: provider %q -> %s (model %s, context %d, max output %d)", openClawProvider, baseURL, alias, ctxWindow, maxOut)
	}
	target := openClawProvider + "/" + alias
	prev, perr := openClawExec(bin, "config", "get", "agents.defaults.model.primary")
	prev = strings.TrimSpace(lastLine(prev))
	if perr != nil {
		prev = "" // no default model configured
	}
	if prev == target {
		ui.Info("OpenClaw: default model already %s", target)
		return restore
	}
	if _, err := openClawExec(bin, "config", "set", "agents.defaults.model.primary", target); err != nil {
		ui.Warn("OpenClaw: could not set the default model: %v", err)
		return restore
	}
	ui.Info("OpenClaw: default model %s for this session (was %s; restored on exit)", target, orNone(prev))
	ui.Dim("OpenClaw's gateway reads its config at start: if the TUI says 'gateway disconnected' run `openclaw gateway run` in another terminal; if it was already running, `openclaw gateway restart`")
	done := false
	restore = func() {
		if done {
			return
		}
		done = true
		var err error
		if prev == "" {
			_, err = openClawExec(bin, "config", "unset", "agents.defaults.model.primary")
		} else {
			_, err = openClawExec(bin, "config", "set", "agents.defaults.model.primary", prev)
		}
		if err != nil {
			ui.Warn("OpenClaw: could not restore the default model (%s): %v - run: openclaw models set %s", orNone(prev), err, orNone(prev))
			return
		}
		ui.Info("OpenClaw: default model restored to %s", orNone(prev))
	}
	return restore
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ---- shared openclaw CLI plumbing (also used by the MCP registration) ----

// openClawServerJSON is the mcp.servers entry winc keeps in OpenClaw's config:
// the stdio form `openclaw mcp set` documents ({"command":..., "args":[...]}).
func openClawServerJSON(exe string) string {
	// Forward slashes: the JSON crosses cmd.exe and node argv parsing, which eat
	// backslashes next to quotes; Windows launches "C:/x/winc.exe" just the same.
	b, _ := json.Marshal(map[string]any{"command": filepath.ToSlash(exe), "args": []string{"mcp-search"}})
	return string(b)
}

// registerOpenClawSearch sets mcp.servers.winc in OpenClaw's config through
// its own CLI, skipping the write when the configured entry already matches --
// a launch never rewrites the user's openclaw.json needlessly. OpenClaw exposes
// the tool as winc__web_search.
func registerOpenClawSearch(exe string) error {
	bin, ok := agent.Resolve("openclaw")
	if !ok {
		return errors.New("openclaw not found")
	}
	want := openClawServerJSON(exe)
	if cur, err := openClawExec(bin, "mcp", "show", openClawProvider); err == nil && openClawEntryMatches(cur, want) {
		return nil
	}
	_, err := openClawExec(bin, "mcp", "set", openClawProvider, want)
	return err
}

// openClawRun runs an openclaw CLI command (via cmd.exe for the npm .cmd shim
// on Windows) and returns its combined output.
func openClawRun(bin string, args ...string) (string, error) {
	var c *exec.Cmd
	switch strings.ToLower(filepath.Ext(bin)) {
	case ".cmd", ".bat":
		c = exec.Command("cmd", append([]string{"/c", bin}, args...)...)
	default:
		c = exec.Command(bin, args...)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// openClawEntryMatches reports whether `openclaw mcp show winc` output carries
// the JSON object we want (same command + args), tolerating the banner lines
// and key order the CLI prints.
func openClawEntryMatches(shown, want string) bool {
	i := strings.Index(shown, "{")
	j := strings.LastIndex(shown, "}")
	if i < 0 || j <= i {
		return false
	}
	var got, exp map[string]any
	if json.Unmarshal([]byte(shown[i:j+1]), &got) != nil || json.Unmarshal([]byte(want), &exp) != nil {
		return false
	}
	// `show <name>` prints the entry itself; `show` (all) nests it under the name.
	if inner, ok := got[openClawProvider].(map[string]any); ok {
		got = inner
	}
	gb, _ := json.Marshal(map[string]any{"command": got["command"], "args": got["args"]})
	eb, _ := json.Marshal(map[string]any{"command": exp["command"], "args": exp["args"]})
	return string(gb) == string(eb)
}

// openClawSearchTool is the name OpenClaw gives winc's tool (server prefix +
// double underscore), for messages.
const openClawSearchTool = openClawProvider + "__web_search"
