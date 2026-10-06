package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The bucket names platforms the Node way; a wrong key means "no build" for a
// machine that has one.
func TestPlatformFor(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		musl         bool
		want         string
	}{
		{"darwin", "arm64", false, "darwin-arm64"},
		{"darwin", "amd64", false, "darwin-x64"},
		{"linux", "amd64", false, "linux-x64"},
		{"linux", "arm64", true, "linux-arm64-musl"},
		{"windows", "amd64", false, "win32-x64"},
		{"windows", "arm64", false, "win32-arm64"},
	} {
		got, err := platformFor(c.goos, c.goarch, c.musl)
		if err != nil || got != c.want {
			t.Errorf("platformFor(%s,%s,%v) = %q, %v; want %q", c.goos, c.goarch, c.musl, got, err, c.want)
		}
	}
	if _, err := platformFor("linux", "386", false); err == nil {
		t.Error("386 should have no build")
	}
	if _, err := platformFor("freebsd", "amd64", false); err == nil {
		t.Error("freebsd should have no build")
	}
}

func TestValidInstallTarget(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "stable": true, "latest": true, "2.1.100": true, "2.1.100-beta.1": true,
		"nightly": false, "2.1": false, "../x": false, "v2.1.100": false,
	} {
		if got := ValidInstallTarget(s); got != want {
			t.Errorf("ValidInstallTarget(%q) = %v, want %v", s, got, want)
		}
	}
}

// releaseServer fakes the bucket: /latest, /<v>/manifest.json, /<v>/<plat>/<bin>.
// checksum overrides the manifest's digest when non-empty (mismatch test).
func releaseServer(t *testing.T, version, body, checksum string) *httptest.Server {
	t.Helper()
	plat, err := ClaudePlatform()
	if err != nil {
		t.Skip(err)
	}
	bin := "claude"
	if runtime.GOOS == "windows" {
		bin = "claude.exe"
	}
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	if checksum != "" {
		digest = checksum
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version) // trailing newline, like the real endpoint
	})
	mux.HandleFunc("/"+strings.TrimSpace(version)+"/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"platforms":{%q:{"binary":%q,"checksum":%q,"size":%d}}}`,
			strings.TrimSpace(version), plat, bin, digest, len(body))
	})
	mux.HandleFunc("/"+strings.TrimSpace(version)+"/"+plat+"/"+bin, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, bin, time.Time{}, strings.NewReader(body))
	})
	return httptest.NewServer(mux)
}

func TestFetchClaudeReleaseVerified(t *testing.T) {
	srv := releaseServer(t, "9.9.9", "fake claude binary bytes", "")
	defer srv.Close()
	dir := t.TempDir()
	p, v, err := FetchClaudeRelease(srv.URL, dir)
	if err != nil {
		t.Fatalf("FetchClaudeRelease: %v", err)
	}
	if v != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", v)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "fake claude binary bytes" {
		t.Fatalf("downloaded content = %q, %v", data, err)
	}
	if !strings.Contains(filepath.Base(p), "claude-9.9.9-") {
		t.Errorf("file name %q should carry version + platform", filepath.Base(p))
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode()&0o111 == 0 {
			t.Error("downloaded binary is not executable")
		}
	}
}

// A digest mismatch is a hard fail AND the file must not survive: the next
// step would execute it.
func TestFetchClaudeReleaseDigestMismatch(t *testing.T) {
	srv := releaseServer(t, "9.9.9", "tampered bytes", strings.Repeat("0", 64))
	defer srv.Close()
	dir := t.TempDir()
	p, _, err := FetchClaudeRelease(srv.URL, dir)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v, want digest mismatch", err)
	}
	if p != "" {
		t.Errorf("path %q returned on failure", p)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".part") && !strings.HasSuffix(e.Name(), ".etag") {
			t.Errorf("rejected download left behind: %s", e.Name())
		}
	}
}

// An error page where a version should be must never become a URL segment.
func TestLatestClaudeVersionRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "<html>denied</html>", "latest", "2.1", "../../etc"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		_, err := LatestClaudeVersion(srv.URL)
		srv.Close()
		if err == nil {
			t.Errorf("body %q accepted as a version", body)
		}
	}
}

func TestFetchClaudeReleaseNoPlatform(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "1.0.0") })
	mux.HandleFunc("/1.0.0/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"1.0.0","platforms":{"plan9-mips":{"checksum":"ab","size":1}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, _, err := FetchClaudeRelease(srv.URL, t.TempDir()); err == nil || !strings.Contains(err.Error(), "publishes no") {
		t.Fatalf("err = %v, want 'publishes no <platform> build'", err)
	}
}

// A Claude Code installed moments ago is not on this process's PATH yet; the
// native installer's ~/.local/bin must still be found.
func TestResolveFindsNativeInstallDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir()) // empty dir: LookPath fails
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	if _, ok := Resolve("claude"); ok {
		t.Fatal("Resolve found a claude with nothing installed")
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	bin := filepath.Join(home, ".local", "bin", name)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := Resolve("claude")
	if !ok || got != bin {
		t.Fatalf("Resolve = %q, %v; want %q", got, ok, bin)
	}
	if _, ok := Resolve("not-an-agent"); ok {
		t.Error("unknown app resolved")
	}
}

// Live gate (opt-in: WINC_LIVE=1): the real bucket, the real manifest, the
// real ~250 MB binary, verified and then asked for its version. Proves the URL
// layout winc reproduces is the one Anthropic publishes today.
func TestFetchClaudeReleaseLive(t *testing.T) {
	if os.Getenv("WINC_LIVE") == "" {
		t.Skip("set WINC_LIVE=1 to download the real Claude Code release")
	}
	dir := t.TempDir()
	p, v, err := FetchClaudeRelease(ReleaseBase, dir)
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		t.Fatalf("downloaded binary --version: %v", err)
	}
	if !strings.Contains(string(out), v) {
		t.Fatalf("binary reports %q, manifest says %q", strings.TrimSpace(string(out)), v)
	}
	t.Logf("live: %s -> %s", v, strings.TrimSpace(string(out)))
}
