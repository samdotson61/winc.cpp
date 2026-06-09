package cli

// winc doctor + winc logs: support and diagnostics. Everything here is read-only --
// it stats files, searches PATH, and makes one bare TCP connect to the configured
// port, but never executes engine binaries and never touches a running process.

import (
	"archive/zip"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"winc/internal/config"
	"winc/internal/download"
	"winc/internal/engine"
	"winc/internal/paths"
	"winc/internal/platform"
	"winc/internal/ui"
)

// knownLogs is every log file winc writes; all live in the install dir.
var knownLogs = []string{
	"llama-server.log",
	"llama-swap.log",
	"winc-router.log",
	"worker-haiku.log",
	"worker-mid.log",
	"worker-sonnet.log",
}

// cmdDoctor prints a one-shot health snapshot for bug reports and self-diagnosis.
func cmdDoctor() int {
	cfg := loadConfig()
	for _, line := range doctorReport(cfg) {
		ui.Say("%s", line)
	}
	return 0
}

// doctorReport builds the doctor output as plain lines (shared by `winc doctor`
// and the `winc logs --bundle` support archive).
func doctorReport(cfg *config.Config) []string {
	var L []string
	add := func(format string, a ...any) { L = append(L, fmt.Sprintf(format, a...)) }

	add("winc doctor")
	add("")
	add("winc:     %s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
	add("install:  %s", paths.InstallDir())
	if fileExists(paths.ConfigPath()) {
		add("config:   %s", paths.ConfigPath())
	} else {
		add("config:   %s (missing - defaults in effect; run 'winc setup')", paths.ConfigPath())
	}

	add("")
	add("hardware:")
	hw := platform.DetectHardware()
	if hw.RAMMB > 0 {
		add("  RAM:     %d MB", hw.RAMMB)
	} else {
		add("  RAM:     unknown")
	}
	switch {
	case hw.GPUVendor == "" || hw.GPUVendor == "none":
		add("  GPU:     none detected (CPU-only)")
	case hw.CudaMajor > 0:
		add("  GPU:     %s (%s, %d MB VRAM, CUDA %d.%d)", hw.GPUName, hw.GPUVendor, hw.VRAMMB, hw.CudaMajor, hw.CudaMinor)
	default:
		add("  GPU:     %s (%s, %d MB VRAM)", hw.GPUName, hw.GPUVendor, hw.VRAMMB)
	}
	if hw.Unified {
		add("  unified memory: yes (GPU shares system RAM)")
	}
	add("  engine backend: %s   model memory budget: %d MB", platform.DefaultBackend(hw), hw.MemoryBudgetMB())
	if smallRAM(hw) {
		add("  small-RAM system: team workers run fewer, deeper slots")
	}

	add("")
	add("engine (checked on disk only; never executed by doctor):")
	addBin := func(label, p, missing string) {
		if p == "" {
			add("  %-13s missing - %s", label+":", missing)
			return
		}
		if fi, err := os.Stat(p); err == nil {
			add("  %-13s %s (%d MB, modified %s)", label+":", p, fi.Size()/(1024*1024), fi.ModTime().Format("2006-01-02"))
		} else {
			add("  %-13s %s (unreadable: %v)", label+":", p, err)
		}
	}
	addBin("llama-server", engine.LlamaServerPath(), "run 'winc setup' to install the engine")
	addBin("llama-cli", engine.LlamaCliPath(), "optional; only used by 'winc -s cli'")
	addBin("llama-swap", engine.LlamaSwapPath(), "optional; only used by --multi")

	md := modelsDir(cfg)
	add("")
	add("models (%s):", md)
	entries, derr := os.ReadDir(md)
	if derr != nil {
		add("  (cannot read models dir: %v)", derr)
	}
	nModels := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			continue
		}
		nModels++
		p := filepath.Join(md, e.Name())
		fi, err := e.Info()
		sizeMB := int64(0)
		if err == nil {
			sizeMB = fi.Size() / (1024 * 1024)
		}
		if download.ValidGGUF(p) {
			add("  %-40s %6d MB  ok", e.Name(), sizeMB)
		} else {
			add("  %-40s %6d MB  CORRUPT (bad GGUF header) - delete and re-download", e.Name(), sizeMB)
		}
	}
	if nModels == 0 && derr == nil {
		add("  (none downloaded - 'winc ls' shows the catalogue)")
	}

	add("")
	add("config snapshot:")
	add("  default:   %s on %s", cfg.General.DefaultApp, cfg.General.DefaultModel)
	add("  reasoning: %s", cfg.Reasoning.Mode)
	add("  team:      mode=%s subagents=%s (haiku=%s mid=%s sonnet=%s)",
		cfg.Team.Mode, cfg.Team.Subagents, cfg.Team.Haiku, cfg.Team.Mid, cfg.Team.Sonnet)
	switch {
	case cfg.HuggingFace.Token != "":
		add("  hf token:  set in winc.toml (redacted)")
	case os.Getenv("HF_TOKEN") != "":
		add("  hf token:  set via HF_TOKEN env")
	default:
		add("  hf token:  not set (only needed for gated repos)")
	}

	add("")
	add("agents on PATH:")
	for _, a := range []string{"claude", "opencode", "openclaw"} {
		if p, err := exec.LookPath(a); err == nil {
			add("  %-9s %s", a+":", p)
		} else {
			add("  %-9s not found", a+":")
		}
	}

	add("")
	addr := net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port)) // IPv6-safe (vet: hostport)
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		conn.Close()
		add("port %d:  in use - something is listening on %s (doctor only connects; it never identifies or touches the process)", cfg.General.Port, addr)
	} else {
		add("port %d:  free", cfg.General.Port)
	}

	add("")
	add("logs (%s):", paths.InstallDir())
	for _, name := range knownLogs {
		p := filepath.Join(paths.InstallDir(), name)
		if fi, err := os.Stat(p); err == nil {
			add("  %-20s %8.1f KB  %s", name, float64(fi.Size())/1024, fi.ModTime().Format("2006-01-02 15:04"))
		} else {
			add("  %-20s -", name)
		}
	}
	add("")
	add("share a full snapshot with:  winc logs --bundle")
	return L
}

// cmdLogs shows the tail of winc's log files, or bundles everything into a support
// zip. Usage: winc logs [name-filter] [--bundle]
func cmdLogs(args []string) int {
	bundle := false
	filter := ""
	for _, a := range args {
		if a == "--bundle" || a == "-b" {
			bundle = true
			continue
		}
		filter = strings.ToLower(a)
	}
	if bundle {
		return writeSupportBundle(loadConfig())
	}

	shown := 0
	for _, name := range knownLogs {
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		p := filepath.Join(paths.InstallDir(), name)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		shown++
		ui.Good("%s (%0.1f KB, %s)", name, float64(fi.Size())/1024, fi.ModTime().Format("2006-01-02 15:04"))
		for _, line := range tailLines(p, 40) {
			ui.Say("  %s", line)
		}
		ui.Say("")
	}
	if shown == 0 {
		if filter != "" {
			ui.Warn("no log matches %q (known: %s)", filter, strings.Join(knownLogs, ", "))
		} else {
			ui.Info("no logs yet - they appear in %s after the first 'winc -s' run", paths.InstallDir())
		}
	}
	return 0
}

// tailLines returns the last n lines of a file (whole file if shorter).
func tailLines(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("(unreadable: %v)", err)}
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// tokenLine matches a TOML `token = "..."` line so the support bundle never
// carries the HuggingFace token.
var tokenLine = regexp.MustCompile(`(?m)^(\s*token\s*=\s*)"[^"]*"`)

// redactConfig blanks secrets out of a winc.toml for sharing.
func redactConfig(raw []byte) []byte {
	return tokenLine.ReplaceAll(raw, []byte(`${1}"<redacted>"`))
}

// writeSupportBundle zips every log, the doctor report, and a token-redacted
// winc.toml into ./winc-support-<timestamp>.zip -- one attachable file per issue.
func writeSupportBundle(cfg *config.Config) int {
	name := fmt.Sprintf("winc-support-%s.zip", time.Now().Format("20060102-150405"))
	f, err := os.Create(name)
	if err != nil {
		ui.Err("cannot create %s: %v", name, err)
		return 1
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	addBytes := func(entry string, data []byte) {
		w, err := zw.Create(entry)
		if err == nil {
			_, _ = w.Write(data)
		}
	}
	addBytes("doctor-report.txt", []byte(strings.Join(doctorReport(cfg), "\n")+"\n"))
	if raw, err := os.ReadFile(paths.ConfigPath()); err == nil {
		addBytes("winc.toml", redactConfig(raw))
	}
	n := 0
	for _, lg := range knownLogs {
		p := filepath.Join(paths.InstallDir(), lg)
		src, err := os.Open(p)
		if err != nil {
			continue
		}
		if w, err := zw.Create(lg); err == nil {
			_, _ = io.Copy(w, src)
			n++
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		ui.Err("bundle failed: %v", err)
		return 1
	}
	ui.Good("wrote %s (doctor report + redacted winc.toml + %d log file(s))", name, n)
	ui.Say("  attach it to a GitHub issue: https://github.com/samdotson61/winc.cpp/issues")
	return 0
}
