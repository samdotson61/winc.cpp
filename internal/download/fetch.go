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
)

var client = &http.Client{} // streaming; no overall timeout

// Fetch streams url -> dest with a 1s progress bar, Range resume, optional
// headers, and atomic rename (writes dest+".part" then renames).
func Fetch(url, dest string, headers map[string]string, label string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
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
		startAt = 0 // server ignored Range; restart cleanly
		flag |= os.O_TRUNC
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
	pr := newProgress(total, label)
	pr.done = startAt
	if _, err := io.Copy(io.MultiWriter(f, pr), resp.Body); err != nil {
		f.Close()
		return err
	}
	pr.finish()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dest)
}
