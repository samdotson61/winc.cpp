//go:build darwin

package platform

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func processStamp(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	// comm may be a full path; lstart is a fixed-format date after it.
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return ""
	}
	return filepath.Base(parts[0]) + "|" + strings.Join(parts[1:], " ")
}
