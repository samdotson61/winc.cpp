package download

import (
	"fmt"
	"os"
	"path/filepath"
)

// HFDownload fetches one GGUF from a HuggingFace repo into destDir. It is
// idempotent: if the file already exists it returns immediately. Honors HF_TOKEN
// (or the passed token) for gated repos; Go's HTTP client drops the auth header
// on cross-host CDN redirects automatically, matching huggingface_hub behavior.
func HFDownload(repo, file, destDir, token string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, filepath.Base(file))
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
	if err := Fetch(url, dest, headers, fmt.Sprintf("%s  (%s)", filepath.Base(file), repo)); err != nil {
		return "", err
	}
	return dest, nil
}
