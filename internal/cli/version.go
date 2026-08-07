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
// The suffix also keeps `winc update`'s source rebuild from stamping master's
// git-describe over it (update.go skips stamping when the literal says jobdar).
var Version = "1.38.0-jobdar.1"

func cmdVersion() int {
	ui.Say("winc %s", Version)
	return 0
}
