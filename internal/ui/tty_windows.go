package ui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
)

// stdinIsTerminal asks the console subsystem directly. The portable
// ModeCharDevice check is wrong here: Windows' NUL device and other
// character devices pass it, so `winc ... < NUL` (and every CI pipe that
// redirects from NUL) would look like a person at a keyboard.
func stdinIsTerminal() bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
