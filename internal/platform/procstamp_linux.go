//go:build linux

package platform

import (
	"fmt"
	"os"
	"strings"
)

func processStamp(pid int) string {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// Field 22 (starttime, clock ticks since boot) — after the parenthesised
	// comm, which can itself contain spaces, so split from the LAST ')'.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(s[i+1:]) // fields[0] is field 3 (state)
	if len(fields) < 20 {
		return ""
	}
	return strings.TrimSpace(string(comm)) + "|" + fields[19]
}
