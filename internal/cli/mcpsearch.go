package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

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
		ui.Info("web search: %s via %s (OpenClaw MCP server \"winc\"; its built-in web_search also works locally)", openClawSearchTool, prov)
	}
	return nil, nil
}

// searchToolForNotes is the tool name WriteAgentNotes should advertise, or ""
// when search is off.
func searchToolForNotes(cfg *config.Config) string {
	if search.Enabled(cfg.Search) {
		return search.ToolName
	}
	return ""
}
