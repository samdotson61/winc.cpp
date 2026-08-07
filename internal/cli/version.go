package cli

import "winc/internal/ui"

// Version is the winc release version. It is a var (not a const) so release
// builds can stamp the exact git tag via:
//
//	-ldflags "-X winc/internal/cli.Version=1.14.1"
//
// No "v" prefix -- the update check compares against tags with the "v"
// stripped. A plain `go build` keeps this default.
// The "-jobdar.N" suffix is LOAD-BEARING, not cosmetic: selfUpdatePrebuilt
// refuses to replace a build whose Version contains "jobdar" (update.go), which
// is what stops a jobfaro backend from silently becoming a master release.
var Version = "1.34.0-jobdar.1"

func cmdVersion() int {
	ui.Say("winc %s", Version)
	return 0
}
