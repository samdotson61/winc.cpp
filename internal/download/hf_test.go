package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLFSPointer(t *testing.T) {
	ptr := "version https://git-lfs.github.com/spec/v1\noid sha256:abc123\nsize 42\n"
	p, ok := parseLFSPointer([]byte(ptr))
	if !ok || p.sha256 != "abc123" || p.size != 42 {
		t.Fatalf("parse = %+v, %v", p, ok)
	}
	if _, ok := parseLFSPointer([]byte("GGUF\x03binary-not-a-pointer")); ok {
		t.Fatal("binary content parsed as a pointer")
	}
	if _, ok := parseLFSPointer([]byte("version https://git-lfs.github.com/spec/v1\nsize 42\n")); ok {
		t.Fatal("pointer without oid accepted")
	}
}

// hfServer serves a fake HF repo: the model bytes at /resolve/main/ and, when
// pointer != "", a git-lfs pointer at /raw/main/ (else 404, the non-LFS path).
func hfServer(t *testing.T, content, pointer string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/repo/resolve/main/m.gguf", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	})
	mux.HandleFunc("/org/repo/raw/main/m.gguf", func(w http.ResponseWriter, r *http.Request) {
		if pointer == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, pointer)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func lfsPointerFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		hex.EncodeToString(sum[:]), len(content))
}

// The happy path: download, GGUF header check, and sha256 verification against
// the published pointer all pass.
func TestHFDownloadVerified(t *testing.T) {
	content := "GGUF" + strings.Repeat("x", 64)
	srv := hfServer(t, content, lfsPointerFor(content))
	t.Setenv("HF_ENDPOINT", srv.URL)

	dest, err := HFDownload("org/repo", "m.gguf", t.TempDir(), "")
	if err != nil {
		t.Fatalf("HFDownload: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != content {
		t.Fatalf("content mismatch")
	}
}

// A published digest that doesn't match the delivered bytes must remove the
// file and fail loudly.
func TestHFDownloadVerifyMismatch(t *testing.T) {
	content := "GGUF" + strings.Repeat("x", 64)
	srv := hfServer(t, content, lfsPointerFor("GGUF-different-bytes"))
	t.Setenv("HF_ENDPOINT", srv.URL)

	dir := t.TempDir()
	_, err := HFDownload("org/repo", "m.gguf", dir, "")
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("want verification error, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "m.gguf")); !os.IsNotExist(serr) {
		t.Fatal("failed-verification file left on disk")
	}
}

// No reachable pointer (offline mirror, non-LFS file) skips verification: the
// download itself already passed the GGUF header check and must succeed.
func TestHFDownloadPointerUnavailable(t *testing.T) {
	content := "GGUF" + strings.Repeat("x", 64)
	srv := hfServer(t, content, "")
	t.Setenv("HF_ENDPOINT", srv.URL)

	if _, err := HFDownload("org/repo", "m.gguf", t.TempDir(), ""); err != nil {
		t.Fatalf("HFDownload without pointer: %v", err)
	}
}

// A 200 that delivers an error page instead of a model must be caught by the
// GGUF header check and removed (pre-existing behavior, now locked by a test).
func TestHFDownloadRejectsNonGGUF(t *testing.T) {
	content := "<html>please sign in</html>"
	srv := hfServer(t, content, "")
	t.Setenv("HF_ENDPOINT", srv.URL)

	dir := t.TempDir()
	_, err := HFDownload("org/repo", "m.gguf", dir, "")
	if err == nil || !strings.Contains(err.Error(), "GGUF") {
		t.Fatalf("want bad-GGUF error, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "m.gguf")); !os.IsNotExist(serr) {
		t.Fatal("non-GGUF file left on disk")
	}
}
