package download

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fresh download lands atomically and leaves no .part/.etag sidecars behind.
func TestFetchFresh(t *testing.T) {
	const body = "fresh-content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("fresh download sent Range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	if err := Fetch(srv.URL, dest, nil, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != body {
		t.Fatalf("dest = %q, %v; want %q", got, err, body)
	}
	for _, side := range []string{dest + ".part", dest + ".part.etag"} {
		if _, err := os.Stat(side); !os.IsNotExist(err) {
			t.Errorf("%s left behind", side)
		}
	}
}

// A truncated transfer must fail, keep the .part + saved ETag for resume, and
// the follow-up request must offer that ETag via If-Range; the server honoring
// it with a 206 completes the file from where it stopped.
func TestFetchTruncatedThenResume(t *testing.T) {
	const full = "0123456789abcdef"
	const cut = 6 // bytes delivered before the first attempt "dies"
	var calls int
	var gotRange, gotIfRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Declare the full length but deliver a prefix: the client sees the
			// connection end early and the transfer must NOT be renamed complete.
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			w.Write([]byte(full[:cut]))
			return
		}
		gotRange = r.Header.Get("Range")
		gotIfRange = r.Header.Get("If-Range")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", cut, len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, full[cut:])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	if err := Fetch(srv.URL, dest, nil, ""); err == nil {
		t.Fatal("truncated download did not error")
	}
	if fi, err := os.Stat(dest + ".part"); err != nil || fi.Size() != cut {
		t.Fatalf(".part after truncation = %v, %v; want %d bytes", fi, err, cut)
	}
	if etag, err := os.ReadFile(dest + ".part.etag"); err != nil || string(etag) != `"v1"` {
		t.Fatalf("saved etag = %q, %v; want %q", etag, err, `"v1"`)
	}

	if err := Fetch(srv.URL, dest, nil, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if gotRange != fmt.Sprintf("bytes=%d-", cut) {
		t.Errorf("resume Range = %q", gotRange)
	}
	if gotIfRange != `"v1"` {
		t.Errorf("resume If-Range = %q", gotIfRange)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != full {
		t.Fatalf("resumed dest = %q; want %q", got, full)
	}
}

// If the remote file changed between attempts (If-Range mismatch), the server
// answers 200 with the whole new body -- the stale partial must be discarded,
// never spliced.
func TestFetchResumeRemoteChanged(t *testing.T) {
	const fresh = "entirely-new-content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Range") == "" {
			t.Error("resume with a saved etag sent no If-Range")
		}
		w.Header().Set("ETag", `"v2"`)
		fmt.Fprint(w, fresh) // full 200 body: validator mismatch path
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(dest+".part", []byte("stale-old-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part.etag", []byte(`"v1"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(srv.URL, dest, nil, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != fresh {
		t.Fatalf("dest = %q; want the fresh body %q (stale partial spliced?)", got, fresh)
	}
}

// A connection that goes silent mid-body is aborted by the stall watchdog with
// a clear error, promptly, and the .part survives for resume.
func TestFetchStallAborts(t *testing.T) {
	old := stallTimeout
	stallTimeout = 150 * time.Millisecond
	defer func() { stallTimeout = old }()

	const sent = 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write(make([]byte, sent))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hang until the watchdog closes the client side
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	start := time.Now()
	err := Fetch(srv.URL, dest, nil, "")
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("want stall error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("watchdog took %v", elapsed)
	}
	if fi, serr := os.Stat(dest + ".part"); serr != nil || fi.Size() != sent {
		t.Fatalf(".part after stall = %v, %v; want %d bytes kept for resume", fi, serr, sent)
	}
}
