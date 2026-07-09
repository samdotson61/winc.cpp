// Package download streams files over HTTP with a live progress bar, HTTP Range
// resume, and atomic rename. Used for both GGUF models (HuggingFace) and engine
// archives (GitHub releases). No Python, no external tools.
package download

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// client streams multi-GB bodies, so it carries NO overall timeout -- but every
// phase that can hang silently is bounded: TLS handshake, response headers, and
// (via stallGuard) the gap between body reads.
var client = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 15 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}()}

// stallTimeout aborts a transfer whose connection has gone silent -- no bytes
// for this long is a dead peer, not a slow one. The .part survives, so the
// retry resumes instead of restarting. A var so tests can shorten it.
var stallTimeout = 30 * time.Second

// stallGuard wraps a response body and closes it when a Read makes no progress
// for stallTimeout, turning a silent TCP hang into a prompt, resumable error.
type stallGuard struct {
	body  io.ReadCloser
	timer *time.Timer
	fired atomic.Bool
}

func newStallGuard(body io.ReadCloser) *stallGuard {
	g := &stallGuard{body: body}
	g.timer = time.AfterFunc(stallTimeout, func() {
		g.fired.Store(true)
		g.body.Close() // unblocks the pending Read
	})
	return g
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 && !g.fired.Load() {
		g.timer.Reset(stallTimeout)
	}
	return n, err
}

func (g *stallGuard) stop() { g.timer.Stop() }

// Fetch streams url -> dest with a 1s progress bar, Range resume, optional
// headers, and atomic rename (writes dest+".part" then renames). A resume is
// validated with If-Range against the ETag saved beside the .part: if the
// remote file changed between attempts, the server sends the whole file again
// (200) and the stale partial is discarded instead of spliced.
func Fetch(url, dest string, headers map[string]string, label string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	etagPath := part + ".etag"
	var startAt int64
	if fi, err := os.Stat(part); err == nil {
		startAt = fi.Size()
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if startAt > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startAt))
		// Only append to the SAME remote bytes: If-Range makes the server ignore
		// the Range (and send 200 + the full file) when the ETag no longer
		// matches -- e.g. the repo re-uploaded the file between attempts. A .part
		// without a saved ETag (pre-1.22 download) resumes unvalidated, as before.
		if etag, rerr := os.ReadFile(etagPath); rerr == nil {
			if v := strings.TrimSpace(string(etag)); v != "" {
				req.Header.Set("If-Range", v)
			}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	flag := os.O_CREATE | os.O_WRONLY
	if resp.StatusCode == http.StatusPartialContent {
		flag |= os.O_APPEND
	} else {
		startAt = 0 // server sent the full body (no Range, or If-Range said stale); restart cleanly
		flag |= os.O_TRUNC
		// Save this body's validator BEFORE the copy so an interrupted transfer
		// leaves .part and .etag consistent for the next attempt's If-Range.
		if etag := resp.Header.Get("ETag"); etag != "" {
			_ = os.WriteFile(etagPath, []byte(etag), 0o644)
		} else {
			_ = os.Remove(etagPath)
		}
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	if total > 0 {
		total += startAt
	}
	if label != "" {
		fmt.Printf("  %s\n", label)
	}
	guard := newStallGuard(resp.Body)
	pr := newProgress(total, label)
	pr.done = startAt
	written, err := io.Copy(io.MultiWriter(f, pr), guard)
	guard.stop()
	if err != nil {
		f.Close()
		if guard.fired.Load() {
			return fmt.Errorf("download stalled (no data for %s) - re-run to resume", stallTimeout)
		}
		return err
	}
	pr.finish()
	if err := f.Close(); err != nil {
		return err
	}
	// A connection that drops at a chunk boundary ends the stream WITHOUT an error
	// from io.Copy -- only this length check stops a truncated file being renamed
	// into place as if complete. The .part stays, so the next attempt resumes.
	if resp.ContentLength > 0 && written != resp.ContentLength {
		return fmt.Errorf("incomplete download: got %d of %d bytes (re-run to resume)", startAt+written, total)
	}
	_ = os.Remove(etagPath)
	return os.Rename(part, dest)
}
