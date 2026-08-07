package platform

// ProcessStamp returns an opaque identity for a live process: its executable
// base name plus an OS-native start-time token, or "" when the process is gone
// or unreadable. Two calls agree only while the SAME process occupies the PID,
// so a recorded stamp proves a PID hasn't been recycled — the safety predicate
// behind `winc stop`/`winc restart`, which must only ever touch processes winc
// itself started and stamped. The token is compared for equality, never
// parsed, so each OS can use whatever start-time form it has cheap.
func ProcessStamp(pid int) string { return processStamp(pid) }
