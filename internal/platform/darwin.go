//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// OSLabel is a friendly OS name.
func OSLabel() string { return "macOS" }

func detectRAMMB() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return int(b / (1024 * 1024))
}

func detectGPU(hw *Hardware) {
	if runtime.GOARCH == "arm64" {
		hw.GPUVendor = "apple"
		hw.GPUName = "Apple Silicon (Metal)"
		hw.Unified = true
		hw.VRAMMB = hw.RAMMB // unified memory
		return
	}
	if detectNvidia(hw) {
		return
	}
	hw.GPUVendor = "none"
}

// PkgManager returns "brew" if Homebrew is installed, else "".
func PkgManager() string {
	if _, err := exec.LookPath("brew"); err == nil {
		return "brew"
	}
	return ""
}

// InstallPackage installs a formula via Homebrew.
func InstallPackage(name string) error {
	if PkgManager() == "" {
		return fmt.Errorf("Homebrew not found; install it from https://brew.sh then re-run")
	}
	c := exec.Command("brew", "install", name)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// EnsureBuildEnv checks the Xcode Command Line Tools are present (source builds).
func EnsureBuildEnv() error {
	if err := exec.Command("xcode-select", "-p").Run(); err != nil {
		return fmt.Errorf("Xcode Command Line Tools missing; run: xcode-select --install")
	}
	return nil
}

// performanceCores: Apple Silicon publishes the P cluster as perflevel0;
// Intel Macs lack the key entirely -> 0 (uniform cores, engine default is
// right). A perflevel0 that covers every core is not a P/E split either.
func performanceCores() int {
	out, err := exec.Command("sysctl", "-n", "hw.perflevel0.logicalcpu").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 || n >= runtime.NumCPU() {
		return 0
	}
	return n
}

// efficiencyCoreRange returns the [lo, hi] logical-CPU index range of the
// efficiency cluster, or ok=false when there is no P/E split. Apple Silicon
// numbers the P cluster first (0..P-1) and the E cluster last (P..N-1), so the
// E range is [performanceCores(), NumCPU-1]. Used to pin unified-memory team
// workers off the P cores, where CPU worker decode contends with the main GPU
// model for memory bandwidth (measured: 16% main-decode loss, ~half recovered
// by this pin).
func efficiencyCoreRange() (lo, hi int, ok bool) {
	p := performanceCores()
	n := runtime.NumCPU()
	if p <= 0 || p >= n {
		return 0, 0, false
	}
	return p, n - 1, true
}
