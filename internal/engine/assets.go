package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"winc/internal/platform"
)

// llamaBuildRe parses llama-server's version line into the build number. Two
// upstream formats: the classic "version: 9651 (e3bb1add8)" and, since the
// versioned-release scheme (llama.cpp 0.2.0, 2026-08-21), "version: 0.3.0-dev
// (build 10790, commit 8c1a25166)". The explicit "build N" wins when present;
// a bare "version: N" is the old form. Matching the leading digits of "0.3.0"
// would have read every new engine as build 0.
var (
	llamaBuildRe   = regexp.MustCompile(`(?i)\bbuild\s+(\d+)`)
	llamaVersionRe = regexp.MustCompile(`(?i)version:\s*(\d+)\s*\(`)
)

// llamaBuildTag extracts the "bNNNN" tag from a llama-server --version output,
// or "" when neither format is present.
func llamaBuildTag(out []byte) string {
	if m := llamaBuildRe.FindSubmatch(out); m != nil {
		return "b" + string(m[1])
	}
	if m := llamaVersionRe.FindSubmatch(out); m != nil {
		return "b" + string(m[1])
	}
	return ""
}

// InstalledLlamaTag returns the build tag of the installed llama-server (e.g. "b9550"),
// or "" if it isn't installed or the version can't be read.
func InstalledLlamaTag() string {
	return LlamaTagOf(LlamaServerPath())
}

// LlamaTagOf returns the build tag of the given llama-server binary (e.g.
// "b9550"), or "" if bin is empty or the version can't be read.
func LlamaTagOf(bin string) string {
	if bin == "" {
		return ""
	}
	cmd := exec.Command(bin, "--version")
	cmd.Env = mtpProbeEnv(bin) // make co-located shared libs loadable
	out, _ := cmd.CombinedOutput()
	return llamaBuildTag(out)
}

const (
	llamaRepo        = "ggml-org/llama.cpp"
	swapRepo         = "mostlygeek/llama-swap"
	wincRepo         = "samdotson61/winc.cpp"
	llamaFallbackTag = "b10621" // verified 2026-09-03 (the v0.3.0 nightly-tag pick; asset set intact; loads + benches the Metal roster flat vs b9651)
	swapFallbackTag  = "223"    // verified 2026-06-06 (release tag v223)
)

// LatestWincTag returns the newest winc.cpp release tag, or "" if it can't be reached.
func LatestWincTag() string { return latestTag(wincRepo, "") }

// latestTag asks the GitHub releases API; falls back to a known-good tag offline.
func latestTag(repo, fallback string) string { return latestRelease(repo, fallback).Tag }

// releaseInfo is the GitHub release metadata winc uses: the tag, plus each asset's
// published sha256 (the API's per-asset `digest` field, present on releases since 2025).
type releaseInfo struct {
	Tag     string
	Digests map[string]string // asset filename -> "sha256:<hex>"; empty when unavailable
}

var (
	releaseMu    sync.Mutex
	releaseCache = map[string]releaseInfo{} // repo -> fetched release; successes only
)

// latestRelease fetches the newest release's tag and asset digests in one API call.
// Offline (or rate-limited) it falls back to a known-good tag with NO digests --
// callers then download without verification and say so, rather than refusing to
// install at all. A successful fetch is cached for the rest of the run: `winc
// update` asks for the same release from the check, the candidate list, and the
// digest verification, and one answer also keeps those consistent. Failures are
// never cached (a flaky network can recover mid-run, and the fallback tag varies
// by caller).
func latestRelease(repo, fallback string) releaseInfo {
	releaseMu.Lock()
	ri, ok := releaseCache[repo]
	releaseMu.Unlock()
	if ok {
		return ri
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return releaseInfo{Tag: fallback}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{Tag: fallback}
	}
	var r struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || r.TagName == "" {
		return releaseInfo{Tag: fallback}
	}
	ri = releaseInfo{Tag: r.TagName, Digests: map[string]string{}}
	for _, a := range r.Assets {
		if a.Digest != "" {
			ri.Digests[a.Name] = a.Digest
		}
	}
	releaseMu.Lock()
	releaseCache[repo] = ri
	releaseMu.Unlock()
	return ri
}

// LatestLlamaTag / LatestSwapTag expose the newest upstream release tags.
func LatestLlamaTag() string { return latestLlamaRelease().Tag }
func LatestSwapTag() string  { return latestTag(swapRepo, swapFallbackTag) }

// nightlyTagAsset is the pointer file a versioned llama.cpp release carries.
const nightlyTagAsset = "nightly-tag.txt"

// latestLlamaRelease resolves the engine build to install. Since llama.cpp
// 0.2.0 (2026-08-21) upstream's /releases/latest is a VERSIONED release
// ("v0.3.0") whose only asset is nightly-tag.txt naming the bNNNN build it
// blesses; the per-commit bNNNN releases (the ones carrying the binaries) are
// all prereleases, so /latest never returns them any more. Following the
// pointer keeps winc on upstream's recommended build instead of the newest
// commit, and the pointed-at release supplies the asset digests. A pointer
// that can't be read falls back to the pinned tag with no digests, exactly
// like an offline lookup. A bNNNN /latest (the pre-August scheme) still works
// unchanged.
func latestLlamaRelease() releaseInfo {
	rel := latestRelease(llamaRepo, llamaFallbackTag)
	if isBuildTag(rel.Tag) {
		return rel
	}
	key := llamaRepo + "#resolved"
	releaseMu.Lock()
	ri, ok := releaseCache[key]
	releaseMu.Unlock()
	if ok {
		return ri
	}
	tag := readNightlyTag(rel.Tag)
	if !isBuildTag(tag) {
		return releaseInfo{Tag: llamaFallbackTag}
	}
	ri = releaseByTag(llamaRepo, tag)
	releaseMu.Lock()
	releaseCache[key] = ri
	releaseMu.Unlock()
	return ri
}

// isBuildTag reports whether a tag is a per-commit llama.cpp build ("b10790").
func isBuildTag(tag string) bool {
	if len(tag) < 2 || tag[0] != 'b' {
		return false
	}
	for _, c := range tag[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readNightlyTag downloads a versioned release's nightly-tag.txt and returns
// its trimmed content ("" on any failure).
func readNightlyTag(versionTag string) string {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://github.com/" + llamaRepo + "/releases/download/" + versionTag + "/" + nightlyTagAsset)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	return parseNightlyTag(buf[:n])
}

// parseNightlyTag extracts the build tag from nightly-tag.txt content.
func parseNightlyTag(b []byte) string {
	tag := strings.TrimSpace(string(b))
	if i := strings.IndexAny(tag, " \t\r\n"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// releaseByTag fetches one release's asset digests by tag. On failure the tag
// is kept and the digests are empty (download proceeds unverified, as offline).
func releaseByTag(repo, tag string) releaseInfo {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://api.github.com/repos/" + repo + "/releases/tags/" + tag)
	if err != nil {
		return releaseInfo{Tag: tag}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{Tag: tag}
	}
	var r struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return releaseInfo{Tag: tag}
	}
	ri := releaseInfo{Tag: tag, Digests: map[string]string{}}
	for _, a := range r.Assets {
		if a.Digest != "" {
			ri.Digests[a.Name] = a.Digest
		}
	}
	return ri
}

// WincAsset resolves the latest winc release's download URL and published
// sha256 digest for the named asset (digest "" when unpublished). ok=false
// when the release can't be reached -- callers keep the current binary.
func WincAsset(name string) (url, digest string, ok bool) {
	rel := latestRelease(wincRepo, "")
	if rel.Tag == "" {
		return "", "", false
	}
	return "https://github.com/" + wincRepo + "/releases/download/" + rel.Tag + "/" + name, rel.Digests[name], true
}

// LlamaAsset is one downloadable engine archive (plus any runtime companion).
type LlamaAsset struct {
	Backend string
	URLs    []string          // main archive [+ CUDA cudart runtime]
	Archive string            // "zip" | "tar.gz"
	Digests map[string]string // release digests by filename ("sha256:<hex>"); empty = unverifiable
}

// LlamaCandidates returns the ordered prebuilt llama.cpp archives to try for this
// hardware, best backend first, always ending in a CPU fallback that exists.
// NOTE: there is no prebuilt Linux CUDA archive (source build only).
func LlamaCandidates(hw platform.Hardware) []LlamaAsset {
	rel := latestLlamaRelease()
	tag := rel.Tag
	base := "https://github.com/" + llamaRepo + "/releases/download/" + tag + "/"
	mk := func(backend, archive string, files ...string) LlamaAsset {
		urls := make([]string, len(files))
		for i, f := range files {
			urls[i] = base + f
		}
		return LlamaAsset{Backend: backend, URLs: urls, Archive: archive, Digests: rel.Digests}
	}
	var out []LlamaAsset
	switch hw.OS {
	case "windows":
		if hw.Arch == "arm64" {
			// Windows-on-ARM is Snapdragon: the Adreno OpenCL build first (the
			// only GPU path upstream ships for it), native-ARM CPU as fallback.
			// Never offer x64 archives here -- they run, but under emulation.
			out = append(out, mk("opencl-adreno", "zip", "llama-"+tag+"-bin-win-opencl-adreno-arm64.zip"))
			out = append(out, mk("cpu", "zip", "llama-"+tag+"-bin-win-cpu-arm64.zip"))
			return out
		}
		if hw.GPUVendor == "nvidia" {
			// Pick the CUDA build matching the driver. cuda-13.3 needs a newer
			// driver; older drivers (CUDA 12.x) must use cuda-12.4 or its PTX
			// won't load. Unknown -> offer both, newest first.
			cu := hw.CudaMajor
			if cu == 0 || cu >= 13 {
				out = append(out, mk("cuda-13.3", "zip", "llama-"+tag+"-bin-win-cuda-13.3-x64.zip", "cudart-llama-bin-win-cuda-13.3-x64.zip"))
			}
			if cu == 0 || cu == 12 || cu >= 13 {
				out = append(out, mk("cuda-12.4", "zip", "llama-"+tag+"-bin-win-cuda-12.4-x64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip"))
			}
			out = append(out, mk("vulkan", "zip", "llama-"+tag+"-bin-win-vulkan-x64.zip"))
		} else if hw.GPUVendor != "none" && hw.GPUVendor != "" {
			out = append(out, mk("vulkan", "zip", "llama-"+tag+"-bin-win-vulkan-x64.zip"))
		}
		out = append(out, mk("cpu", "zip", "llama-"+tag+"-bin-win-cpu-x64.zip"))
	case "linux":
		if hw.Arch == "arm64" {
			// ARM Linux (Snapdragon X laptops, SBCs): vulkan when a GPU is
			// present, native-ARM CPU always. No ROCm arm64 archive upstream.
			if hw.GPUVendor != "none" && hw.GPUVendor != "" {
				out = append(out, mk("vulkan", "tar.gz", "llama-"+tag+"-bin-ubuntu-vulkan-arm64.tar.gz"))
			}
			out = append(out, mk("cpu", "tar.gz", "llama-"+tag+"-bin-ubuntu-arm64.tar.gz"))
			return out
		}
		if hw.GPUVendor == "amd" {
			out = append(out, mk("rocm", "tar.gz", "llama-"+tag+"-bin-ubuntu-rocm-7.2-x64.tar.gz"))
		}
		if hw.GPUVendor != "none" && hw.GPUVendor != "" {
			out = append(out, mk("vulkan", "tar.gz", "llama-"+tag+"-bin-ubuntu-vulkan-x64.tar.gz"))
		}
		out = append(out, mk("cpu", "tar.gz", "llama-"+tag+"-bin-ubuntu-x64.tar.gz"))
	case "darwin":
		if hw.Arch == "arm64" {
			out = append(out, mk("metal", "tar.gz", "llama-"+tag+"-bin-macos-arm64.tar.gz"))
		} else {
			out = append(out, mk("cpu", "tar.gz", "llama-"+tag+"-bin-macos-x64.tar.gz"))
		}
	}
	return out
}

// SwapAsset returns the prebuilt llama-swap archive URL for this hardware, plus
// the release's published digests for verification (empty when unavailable).
func SwapAsset(hw platform.Hardware) (url, archive string, digests map[string]string, ok bool) {
	rel := latestRelease(swapRepo, swapFallbackTag)
	ver := strings.TrimPrefix(rel.Tag, "v")
	var osName, arch, ext string
	switch hw.OS {
	case "windows":
		osName, arch, ext = "windows", "amd64", "zip"
	case "linux":
		osName, arch, ext = "linux", goArch(hw.Arch), "tar.gz"
	case "darwin":
		osName, arch, ext = "darwin", goArch(hw.Arch), "tar.gz"
	default:
		return "", "", nil, false
	}
	file := fmt.Sprintf("llama-swap_%s_%s_%s.%s", ver, osName, arch, ext)
	url = fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", swapRepo, ver, file)
	return url, ext, rel.Digests, true
}

func goArch(a string) string {
	if a == "arm64" {
		return "arm64"
	}
	return "amd64"
}
