package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"winc/internal/agent"
	"winc/internal/config"
	"winc/internal/paths"
	"winc/internal/search"
	"winc/internal/ui"
)

// cmdMcpSearch is the hidden `winc mcp-search` subcommand: the stdio MCP server
// the sandboxed Claude Code launches (registered as "winc" via --mcp-config at
// every `winc -s claude`). It reads winc.toml's [search] section and serves one
// tool, web_search. `winc mcp-search --query "<q>"` runs a single search from
// the terminal instead -- the way to check a backend/key without an agent.
func cmdMcpSearch(args []string) int {
	cfg := loadConfig()
	for i, a := range args {
		if (a == "--query" || a == "-q") && i+1 < len(args) {
			return runSearchOnce(cfg, strings.Join(args[i+1:], " "))
		}
	}
	backend, err := search.New(cfg.Search)
	if err != nil {
		// Still serve: the tool exists and every call explains the misconfiguration
		// (missing key / URL, unknown provider, or search turned off).
		backend = search.Failing(err)
	}
	search.LogPath = filepath.Join(paths.InstallDir(), "winc-search.log")
	if err := search.Serve(os.Stdin, os.Stdout, backend, cfg.Search.MaxResults); err != nil {
		ui.Err("winc mcp-search: %v", err)
		return 1
	}
	return 0
}

func runSearchOnce(cfg *config.Config, q string) int {
	p, reason := search.ResolveProvider(cfg.Search)
	ui.Info("search provider: %s (%s)", p, reason)
	backend, err := search.New(cfg.Search)
	if err != nil {
		ui.Err("%v", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), search.Timeout+2e9)
	defer cancel()
	rs, err := backend.Search(ctx, q, search.ClampN(0, cfg.Search.MaxResults))
	if err != nil {
		ui.Err("search failed: %v", err)
		return 1
	}
	ui.Say("%s", search.Format(q, rs))
	return 0
}

// registerSearch prepares winc's search tool for an agent launch and returns
// the extra launcher arguments and environment entries it needs. The MCP
// server is always THIS winc binary (regenerated every launch, so a moved
// folder never breaks it). Per agent:
//   - claude:   .claude-local/mcp.json + `--mcp-config <file>` (tool: mcp__winc__web_search)
//   - opencode: .opencode-local/opencode.json + OPENCODE_CONFIG=<file>, which
//     OpenCode merges between its global and project config (tool: winc_web_search);
//     the file also points OpenCode's anthropic provider at winc's endpoint
//   - openclaw: `openclaw mcp set winc {...}` into OpenClaw's own config
//     (mcp.servers.winc) -- OpenClaw has no per-launch config file; the entry
//     is one named server, re-set only when it differs (idempotent)
//
// Nothing is registered when search is off.
func registerSearch(cfg *config.Config, app, baseURL string) (args, env []string) {
	if !search.Enabled(cfg.Search) {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		ui.Warn("web search: can't locate the winc binary (%v) - the search tool is not registered this launch", err)
		return nil, nil
	}
	prov, _ := search.ResolveProvider(cfg.Search)
	switch app {
	case "claude":
		p, err := config.WriteMCPConfig(exe)
		if err != nil {
			ui.Warn("web search: could not write the MCP registration: %v", err)
			return nil, nil
		}
		ui.Info("web search: %s via %s (built-in WebSearch is unavailable on a local model)", search.ToolName, prov)
		return []string{"--mcp-config", p}, nil
	case "opencode":
		// OpenCode's Anthropic client (the AI SDK) appends "/messages" to its base
		// URL -- its default already ends in /v1 -- so winc's endpoint is handed
		// over WITH the /v1 (a bare origin 404s at llama-server: "File Not Found").
		ocBase := ""
		if baseURL != "" {
			ocBase = strings.TrimRight(baseURL, "/") + "/v1"
		}
		p, err := config.WriteOpenCodeConfig(exe, ocBase)
		if err != nil {
			ui.Warn("web search: could not write the OpenCode config: %v", err)
			return nil, nil
		}
		ui.Info("web search: winc_web_search via %s (OPENCODE_CONFIG=%s)", prov, p)
		return nil, []string{"OPENCODE_CONFIG=" + p}
	case "openclaw":
		if err := registerOpenClawSearch(exe); err != nil {
			ui.Warn("web search: could not register with OpenClaw: %v", err)
			return nil, nil
		}
		ui.Info("web search: OpenClaw MCP server \"winc\" via %s (openclaw.json mcp.servers.winc)", prov)
	}
	return nil, nil
}

// openClawServerJSON is the mcp.servers entry winc keeps in OpenClaw's config:
// the stdio form `openclaw mcp set` documents ({"command":..., "args":[...]}).
func openClawServerJSON(exe string) string {
	// Forward slashes: the JSON crosses cmd.exe and node argv parsing, which eat
	// backslashes next to quotes; Windows launches "C:/x/winc.exe" just the same.
	b, _ := json.Marshal(map[string]any{"command": filepath.ToSlash(exe), "args": []string{"mcp-search"}})
	return string(b)
}

// registerOpenClawSearch sets mcp.servers.winc in OpenClaw's config through
// its own CLI (the only writer OpenClaw documents), skipping the write when
// the configured entry already matches -- a launch never rewrites the user's
// openclaw.json needlessly.
func registerOpenClawSearch(exe string) error {
	bin, ok := agent.Resolve("openclaw")
	if !ok {
		return errors.New("openclaw not found")
	}
	want := openClawServerJSON(exe)
	if cur, err := openClawRun(bin, "mcp", "show", "winc"); err == nil && openClawEntryMatches(cur, want) {
		return nil
	}
	_, err := openClawRun(bin, "mcp", "set", "winc", want)
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
	if inner, ok := got["winc"].(map[string]any); ok {
		got = inner
	}
	gb, _ := json.Marshal(map[string]any{"command": got["command"], "args": got["args"]})
	eb, _ := json.Marshal(map[string]any{"command": exp["command"], "args": exp["args"]})
	return string(gb) == string(eb)
}

// searchToolForNotes is the tool name WriteAgentNotes should advertise, or ""
// when search is off.
func searchToolForNotes(cfg *config.Config) string {
	if search.Enabled(cfg.Search) {
		return search.ToolName
	}
	return ""
}
