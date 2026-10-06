package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"winc/internal/download"
	"winc/internal/paths"
)

// ReleaseBase is Anthropic's Claude Code release bucket -- the same one the
// official installers (https://claude.ai/install.sh, install.ps1, install.cmd)
// bootstrap from. Layout: <base>/latest (a version string), then
// <base>/<version>/manifest.json (per-platform sha256 + size) and
// <base>/<version>/<platform>/<binary>. A var so tests can point it at a local
// server.
var ReleaseBase = "https://downloads.claude.ai/claude-code-releases"

// versionRe is the shape the `latest` endpoint must return. Anything else (an
// HTML error page, an empty body) is rejected before it can become a URL.
var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// metaClient fetches the two small release files (version + manifest). The
// binary itself goes through download.Fetch, which carries its own stall guard
// and resume.
var metaClient = &http.Client{Timeout: 30 * time.Second}

// manifest is the subset of <version>/manifest.json winc reads.
type manifest struct {
	Version   string `json:"version"`
	Platforms map[string]struct {
		Binary   string `json:"binary"`
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	} `json:"platforms"`
}

// ClaudePlatform is the release-bucket platform key for this machine
// (darwin-arm64, linux-x64-musl, win32-x64, ...).
func ClaudePlatform() (string, error) {
	return platformFor(runtime.GOOS, runtime.GOARCH, isMusl())
}

// platformFor maps a GOOS/GOARCH pair onto the bucket's naming, which follows
// Node's (x64, not amd64; win32, not windows). Pure, for the table test.
func platformFor(goos, goarch string, musl bool) (string, error) {
	var arch string
	switch goarch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("Claude Code has no %s/%s build", goos, goarch)
	}
	switch goos {
	case "darwin":
		return "darwin-" + arch, nil
	case "linux":
		if musl {
			return "linux-" + arch + "-musl", nil
		}
		return "linux-" + arch, nil
	case "windows":
		return "win32-" + arch, nil
	}
	return "", fmt.Errorf("Claude Code has no %s/%s build", goos, goarch)
}

// isMusl reports a musl-based Linux (Alpine and friends) by the presence of
// the musl dynamic loader -- the same signal the official install.sh uses.
func isMusl() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	m, _ := filepath.Glob("/lib/ld-musl-*.so.1")
	return len(m) > 0
}

// LatestClaudeVersion reads <base>/latest and validates it is a version.
func LatestClaudeVersion(base string) (string, error) {
	body, err := getSmall(base + "/latest")
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(body))
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("unexpected version string %q from %s/latest", truncate(v, 40), base)
	}
	return v, nil
}

func getSmall(url string) ([]byte, error) {
	resp, err := metaClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FetchClaudeRelease downloads the latest Claude Code binary for this platform
// into destDir, verifying its size and sha256 against the release manifest
// (a mismatch is discarded, never kept). It returns the verified file's path
// and the version. Nothing is installed yet -- see InstallClaude.
func FetchClaudeRelease(base, destDir string) (binPath, version string, err error) {
	plat, err := ClaudePlatform()
	if err != nil {
		return "", "", err
	}
	version, err = LatestClaudeVersion(base)
	if err != nil {
		return "", "", fmt.Errorf("could not read the latest Claude Code version: %w", err)
	}
	raw, err := getSmall(base + "/" + version + "/manifest.json")
	if err != nil {
		return "", "", fmt.Errorf("could not read the %s release manifest: %w", version, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", fmt.Errorf("release manifest for %s is not JSON: %w", version, err)
	}
	entry, ok := m.Platforms[plat]
	if !ok || entry.Checksum == "" {
		return "", "", fmt.Errorf("release %s publishes no %s build", version, plat)
	}
	binary := entry.Binary
	if binary == "" {
		binary = "claude"
		if strings.HasPrefix(plat, "win32") {
			binary += ".exe"
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", err
	}
	binPath = filepath.Join(destDir, fmt.Sprintf("claude-%s-%s%s", version, plat, filepath.Ext(binary)))
	url := base + "/" + version + "/" + plat + "/" + binary
	label := fmt.Sprintf("Claude Code %s (%s, %.0f MB)", version, plat, float64(entry.Size)/1e6)
	if err := download.Fetch(url, binPath, nil, label); err != nil {
		return "", "", err
	}
	if err := download.VerifySHA256(binPath, entry.Checksum, entry.Size); err != nil {
		_ = os.Remove(binPath)
		return "", "", fmt.Errorf("release digest mismatch (download discarded): %w", err)
	}
	_ = os.Chmod(binPath, 0o755)
	return binPath, version, nil
}

// InstallClaude installs Claude Code the way the official one-liners do, with
// no shell script in between: fetch + verify the latest release binary, then
// run its own `claude install [target]`, which places the versioned binary
// under ~/.local/share/claude, links ~/.local/bin/claude (claude.exe on
// Windows), adds that directory to the user's PATH, and registers the native
// install so Claude Code's background auto-updater takes over from here.
// target is "" (latest), "stable", "latest" or an exact version -- passed
// through to `claude install` exactly as install.sh passes its argument. The
// installer runs with the user's REAL environment (not winc's sandbox): it is
// a machine-level install shared with their cloud Claude Code.
func InstallClaude(target string) error {
	if !ValidInstallTarget(target) {
		return fmt.Errorf("invalid install target %q (use stable, latest, or a version like 2.1.100)", target)
	}
	tmpDir := filepath.Join(paths.InstallDir(), ".tmp")
	bin, _, err := FetchClaudeRelease(ReleaseBase, tmpDir)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(bin)
		_ = os.Remove(tmpDir) // only when empty -- a resumable .part from another run stays
	}()
	args := []string{"install"}
	if target != "" {
		args = append(args, target)
	}
	c := exec.Command(bin, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("`claude install` failed: %w", err)
	}
	if _, ok := Resolve("claude"); !ok {
		return fmt.Errorf("`claude install` finished but no claude binary was found in the usual places")
	}
	return nil
}

// ValidInstallTarget reports whether s is something `claude install` accepts:
// "" (default channel), a channel name, or an exact version.
func ValidInstallTarget(s string) bool {
	return s == "" || s == "stable" || s == "latest" || versionRe.MatchString(s)
}

// Resolve finds the agent's launcher: on PATH first, then in the places the
// supported installers put it (so a Claude Code installed moments ago -- whose
// PATH entry this process has not inherited -- launches without a new
// terminal, and so a PATH-less shell still finds a working install).
func Resolve(app string) (string, bool) {
	name, _, ok := command(app)
	if !ok {
		return "", false
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	for _, p := range installCandidates(name) {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// installCandidates lists where each supported installer puts a launcher.
func installCandidates(name string) []string {
	home, _ := os.UserHomeDir()
	var c []string
	if runtime.GOOS == "windows" {
		c = append(c, filepath.Join(home, ".local", "bin", name+".exe")) // native installer
		if a := os.Getenv("APPDATA"); a != "" {
			c = append(c, filepath.Join(a, "npm", name+".cmd")) // npm -g
		}
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			c = append(c, filepath.Join(l, "Microsoft", "WinGet", "Links", name+".exe")) // winget
		}
		return c
	}
	c = append(c,
		filepath.Join(home, ".local", "bin", name), // native installer
		"/opt/homebrew/bin/"+name,                  // Homebrew (Apple Silicon)
		"/usr/local/bin/"+name,                     // Homebrew (Intel) / npm -g
		"/usr/bin/"+name,                           // apt / dnf / apk
		filepath.Join(home, ".npm-global", "bin", name),
	)
	return c
}
