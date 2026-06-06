// Command winc is the single, portable winc.cpp binary: it manages local GGUF
// models and launches coding agents (Claude Code, OpenCode, OpenClaw) against a
// local llama.cpp server with native Anthropic serving. No Python, no PowerShell.
package main

import (
	"os"

	"winc/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
