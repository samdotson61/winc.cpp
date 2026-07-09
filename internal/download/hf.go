package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Engine archives and self-updates are sha256-verified against published
	// digests; models get the same guarantee here. HF publishes every LFS file's
	// digest in its git-lfs pointer (served at /raw/main/...): when the pointer is
	// reachable, the downloaded bytes MUST match it. An unreachable pointer
	// (offline mirror, non-LFS file) skips the check with a note rather than
	// failing a download that already passed the GGUF header check.
	if p, ok := hfPointer(endpoint, repo, file, headers); ok {
		fmt.Printf("  verifying sha256 ... ")
		if err := verifyFileSHA256(dest, p.sha256, p.size); err != nil {
			fmt.Println("MISMATCH")
			_ = os.Remove(dest)
			return "", fmt.Errorf("%s failed verification (%v) - removed; re-run to download again", filepath.Base(dest), err)
		}
		fmt.Println("ok")
	} else {
		fmt.Println("  (no published sha256 available - skipping verification)")
	}
	return dest, nil
}

// lfsPointer is the parsed form of a git-lfs pointer -- the tiny text blob
// HuggingFace serves at /raw/main/<file> for every LFS-tracked file, carrying
// the sha256 and byte size of the real content.
type lfsPointer struct {
	sha256 string
	size   int64
}

// parseLFSPointer extracts oid/size from a git-lfs pointer body, or ok=false
// when the body is not a pointer (small non-LFS files come back verbatim).
func parseLFSPointer(body []byte) (lfsPointer, bool) {
	if !bytes.HasPrefix(body, []byte("version https://git-lfs")) {
		return lfsPointer{}, false
	}
	var p lfsPointer
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "oid sha256:"); ok {
			p.sha256 = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(line, "size "); ok {
			p.size, _ = strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		}
	}
	return p, p.sha256 != ""
}

// hfPointer fetches the git-lfs pointer for repo/file, giving the published
// sha256 + size to verify a download against. ok=false when no pointer is
// available (offline mirror, non-LFS file, network error): the caller skips
// verification rather than failing a download that already completed.
func hfPointer(endpoint, repo, file string, headers map[string]string) (lfsPointer, bool) {
	url := fmt.Sprintf("%s/%s/raw/main/%s", endpoint, repo, file)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return lfsPointer{}, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return lfsPointer{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lfsPointer{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return lfsPointer{}, false
	}
	return parseLFSPointer(body)
}

// verifyFileSHA256 streams path through sha256 and compares against the
// published digest (and size, when known). Multi-GB models hash in tens of
// seconds; a mismatch means a corrupt or tampered download.
func verifyFileSHA256(path, want string, size int64) error {
	if fi, err := os.Stat(path); err == nil && size > 0 && fi.Size() != size {
		return fmt.Errorf("size %d != published %d", fi.Size(), size)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 %s != published %s", got, want)
	}
	return nil
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
