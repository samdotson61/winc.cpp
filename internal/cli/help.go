package cli

import "winc/internal/ui"

func usage() {
	ui.Say("")
	ui.Say("winc - local Claude Code models (winc.cpp), one portable binary")
	ui.Say("")
	ui.Say("  winc setup                      first-run install wizard")
	ui.Say("  winc ls                         list downloaded + available models")
	ui.Say("  winc -d <alias>                 download a catalogue model")
	ui.Say("  winc -d <repo> <file>           download any GGUF from HuggingFace")
	ui.Say("  winc -r <model> [-y]            delete a downloaded model")
	ui.Say("  winc -s claude <model>          start Claude Code on a local model")
	ui.Say("  winc -s opencode <model>        start OpenCode")
	ui.Say("  winc -s openclaw <model>        start OpenClaw")
	ui.Say("  winc -s cli <model>             raw llama.cpp chat")
	ui.Say("        [--multi] [--reasoning adaptive|on|off|fixed]")
	ui.Say("  winc serve [--multi]            run the server(s)/router only")
	ui.Say("  winc -c | check                 check for updates")
	ui.Say("  winc -u | update                update engine binaries + winc")
	ui.Say("  winc -n | uninstall [-y]        remove installed components")
	ui.Say("  winc help                       this help")
	ui.Say("")
	ui.Say("  All settings live in one file: winc.toml")
	ui.Say("  <model> is a catalogue alias (see 'winc ls') or part of a filename.")
	ui.Say("")
}
