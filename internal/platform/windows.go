//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

// ExeSuffix is the executable extension for this OS.
func ExeSuffix() string { return ".exe" }

// OSLabel is a friendly OS name.
func OSLabel() string { return "Windows" }

func detectRAMMB() int {
	type memoryStatusEx struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int(m.TotalPhys / (1024 * 1024))
}

func detectGPU(hw *Hardware) {
	if detectNvidia(hw) {
		return
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-CimInstance Win32_VideoController | Select-Object -First 1).Name").Output()
	if err != nil {
		hw.GPUVendor = "none"
		return
	}
	name := strings.TrimSpace(string(out))
	hw.GPUName = name
	switch l := strings.ToLower(name); {
	case strings.Contains(l, "nvidia"), strings.Contains(l, "geforce"), strings.Contains(l, "rtx"):
		hw.GPUVendor = "nvidia"
	case strings.Contains(l, "amd"), strings.Contains(l, "radeon"):
		hw.GPUVendor = "amd"
	case strings.Contains(l, "intel"):
		hw.GPUVendor = "intel"
	default:
		hw.GPUVendor = "none"
	}
}

// EnableVT turns on ANSI escape processing in the Windows console.
func EnableVT() {
	const enableVTProcessing = 0x0004
	const stdOutputHandle uintptr = 0xFFFFFFF5 // (DWORD)-11
	k := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k.NewProc("GetStdHandle").Call(stdOutputHandle)
	var mode uint32
	k.NewProc("GetConsoleMode").Call(h, uintptr(unsafe.Pointer(&mode)))
	k.NewProc("SetConsoleMode").Call(h, uintptr(mode|enableVTProcessing))
}

// ---- PATH (user scope, via the registry-backed [Environment] API) ----------

func userPath() string {
	out, _ := exec.Command("powershell", "-NoProfile", "-Command",
		"[Environment]::GetEnvironmentVariable('PATH','User')").Output()
	return strings.TrimSpace(string(out))
}

func setUserPath(v string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"[Environment]::SetEnvironmentVariable('PATH',$env:WINC_NEWPATH,'User')")
	cmd.Env = append(os.Environ(), "WINC_NEWPATH="+v)
	return cmd.Run()
}

// AddToPath adds dir to the user PATH if absent (idempotent).
func AddToPath(dir string) error {
	cur := userPath()
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimSpace(p), dir) {
			return nil
		}
	}
	next := dir
	if t := strings.TrimRight(cur, ";"); t != "" {
		next = t + ";" + dir
	}
	return setUserPath(next)
}

// RemoveFromPath removes dir from the user PATH (idempotent).
func RemoveFromPath(dir string) error {
	cur := userPath()
	var keep []string
	for _, p := range strings.Split(cur, ";") {
		if p == "" || strings.EqualFold(strings.TrimSpace(p), dir) {
			continue
		}
		keep = append(keep, p)
	}
	return setUserPath(strings.Join(keep, ";"))
}

// OnPath reports whether dir is already on the user PATH.
func OnPath(dir string) bool {
	for _, p := range strings.Split(userPath(), ";") {
		if strings.EqualFold(strings.TrimSpace(p), dir) {
			return true
		}
	}
	return false
}

// ---- dependency install (winget) -------------------------------------------

// PkgManager returns the detected package manager name, or "".
func PkgManager() string {
	if _, err := exec.LookPath("winget"); err == nil {
		return "winget"
	}
	return ""
}

// InstallPackage installs a package by its winget id, streaming output.
func InstallPackage(id string) error {
	if PkgManager() == "" {
		return fmt.Errorf("winget not found; install %q manually", id)
	}
	cmd := exec.Command("winget", "install", "--id", id, "-e",
		"--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// EnsureBuildEnv verifies a source build is possible (cmake present). Prebuilt is
// the default path, so this only matters for the build-from-source fallback.
func EnsureBuildEnv() error {
	if _, err := exec.LookPath("cmake"); err != nil {
		return fmt.Errorf("cmake not found (needed only for building llama.cpp from source); install with: winget install Kitware.CMake")
	}
	return nil
}
