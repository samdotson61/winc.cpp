package cli

import "winc/internal/ui"

// Version is the winc release version.
const Version = "1.1.5"

func cmdVersion() int {
	ui.Say("winc %s", Version)
	return 0
}
