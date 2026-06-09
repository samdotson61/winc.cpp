package download

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HFDownload fetches one GGUF from a HuggingFace repo into destDir. It is
// idempotent: if the file already exists it returns immediately. Honors HF_TOKEN
// (or the passed token) for gated repos; Go's HTTP client drops the auth header
// on cross-host CDN redirects automatically, matching huggingface_hub behavior.
func HFDownload(repo, file, destDir, token string) (string, error) {
	return HFDownloadAs(repo, file, destDir, filepath.Base(file), token)
}

// HFDownloadAs is HFDownload but saves under a chosen local filename (saveAs) rather
// than the repo's basename. This lets winc keep variants that share a repo filename
// distinct on disk (e.g. an MTP build saved as "...-MTP-..."), so it never clobbers
// an already-downloaded standard model with the same name.
func HFDownloadAs(repo, file, destDir, saveAs, token string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	if saveAs == "" {
		saveAs = filepath.Base(file)
	}
	dest := filepath.Join(destDir, filepath.Base(saveAs))
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return dest, nil
	}
	endpoint := os.Getenv("HF_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	url := fmt.Sprintf("%s/%s/resolve/main/%s", endpoint, repo, file)
	headers := map[string]string{"User-Agent": "winc.cpp"}
	if token == "" {
		token = os.Getenv("HF_TOKEN")
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	if err := Fetch(url, dest, headers, fmt.Sprintf("%s  (%s)", filepath.Base(saveAs), repo)); err != nil {
		return "", err
	}
	// An HTTP 200 can still deliver a non-model (an auth or error page saved to disk).
	// Catch it now -- a bad header at engine-load time is a far more confusing failure.
	// Only the bytes just fetched are ever removed; pre-existing files are not touched.
	if strings.HasSuffix(strings.ToLower(dest), ".gguf") && !ValidGGUF(dest) {
		_ = os.Remove(dest)
		return "", fmt.Errorf("%s is not a valid GGUF file (bad header) - removed; check the repo/file name and your HF token", filepath.Base(dest))
	}
	return dest, nil
}

// ggufMagic is the 4-byte header every GGUF file starts with.
var ggufMagic = []byte("GGUF")

// ValidGGUF reports whether path starts with the GGUF magic bytes -- a cheap
// integrity check that catches truncated or HTML-error downloads without
// hashing multi-GB files.
func ValidGGUF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return bytes.Equal(head, ggufMagic)
}
