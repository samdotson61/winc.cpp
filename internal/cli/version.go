package cli

import "winc/internal/ui"

// Version is the winc release version. It is a var (not a const) so release
// builds can stamp the exact git tag via:
//
//	-ldflags "-X winc/internal/cli.Version=1.14.1"
//
// No "v" prefix -- the update check compares against tags with the "v"
// stripped. A plain `go build` keeps this default.
var Version = "1.17.1"

func cmdVersion() int {
	ui.Say("winc %s", Version)
	return 0
}
