//go:build !windows

package ui

import "os"

// stdinIsTerminal: on Unix a terminal is a character device and pipes,
// files and /dev/null... are not -- except /dev/null, which IS a character
// device. Prompts answered from /dev/null read EOF immediately, which Confirm
// turns into its default, so treat that one device as non-interactive too.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}
