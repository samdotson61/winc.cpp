//go:build linux

package platform

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OSLabel is a friendly OS name.
func OSLabel() string { return "Linux" }

func detectRAMMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.Atoi(fields[1]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}

func detectGPU(hw *Hardware) {
	if detectNvidia(hw) {
		return
	}
	// Vendor + name from lspci (falling back to rocminfo presence for AMD).
	if out, err := exec.Command("sh", "-c", "lspci | grep -iE 'vga|3d|display'").Output(); err == nil && len(out) > 0 {
		l := strings.ToLower(string(out))
		switch {
		case strings.Contains(l, "nvidia"):
			hw.GPUVendor = "nvidia"
		case strings.Contains(l, "amd"), strings.Contains(l, "radeon"), strings.Contains(l, "advanced micro"):
			hw.GPUVendor = "amd"
		case strings.Contains(l, "intel"):
			hw.GPUVendor = "intel"
		default:
			hw.GPUVendor = "none"
		}
		hw.GPUName = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	} else if _, e := exec.LookPath("rocminfo"); e == nil {
		hw.GPUVendor = "amd"
		hw.GPUName = "AMD GPU"
	} else {
		hw.GPUVendor = "none"
	}
	// Dedicated VRAM: amdgpu exposes it via sysfs (accurate). Intel/others fall
	// back to 0 here, which the conservative budget logic treats as the smallest
	// tier rather than guessing from shared system RAM.
	hw.VRAMMB = detectVRAMMBSysfs()
}

// detectVRAMMBSysfs reads dedicated VRAM (MB) from amdgpu's
// /sys/class/drm/card*/device/mem_info_vram_total. Returns the largest, or 0.
func detectVRAMMBSysfs() int {
	max := 0
	matches, _ := filepath.Glob("/sys/class/drm/card*/device/mem_info_vram_total")
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			if mb := int(n / (1024 * 1024)); mb > max {
				max = mb
			}
		}
	}
	return max
}

// PkgManager returns the detected system package manager, or "".
func PkgManager() string {
	for _, m := range []string{"apt-get", "dnf", "pacman", "zypper"} {
		if _, err := exec.LookPath(m); err == nil {
			return m
		}
	}
	return ""
}

// InstallPackage installs a package via the detected manager (uses sudo).
func InstallPackage(name string) error {
	var args []string
	switch PkgManager() {
	case "apt-get":
		args = []string{"sudo", "apt-get", "install", "-y", name}
	case "dnf":
		args = []string{"sudo", "dnf", "install", "-y", name}
	case "pacman":
		args = []string{"sudo", "pacman", "-S", "--noconfirm", name}
	case "zypper":
		args = []string{"sudo", "zypper", "install", "-y", name}
	default:
		return fmt.Errorf("no supported package manager; install %q manually", name)
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// EnsureBuildEnv checks a source build is possible (only needed for source builds).
func EnsureBuildEnv() error {
	for _, t := range []string{"cmake", "cc"} {
		if _, err := exec.LookPath(t); err != nil {
			return fmt.Errorf("%s not found (needed only for source builds); install build-essential + cmake", t)
		}
	}
	return nil
}
