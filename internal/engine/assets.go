package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"winc/internal/platform"
)

const (
	llamaRepo        = "ggml-org/llama.cpp"
	swapRepo         = "mostlygeek/llama-swap"
	llamaFallbackTag = "b9542" // verified 2026-06-06
	swapFallbackTag  = "223"   // verified 2026-06-06 (release tag v223)
)

// latestTag asks the GitHub releases API; falls back to a known-good tag offline.
func latestTag(repo, fallback string) string {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallback
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || r.TagName == "" {
		return fallback
	}
	return r.TagName
}

// LatestLlamaTag / LatestSwapTag expose the newest upstream release tags.
func LatestLlamaTag() string { return latestTag(llamaRepo, llamaFallbackTag) }
func LatestSwapTag() string  { return latestTag(swapRepo, swapFallbackTag) }

// LlamaAsset is one downloadable engine archive (plus any runtime companion).
type LlamaAsset struct {
	Backend string
	URLs    []string // main archive [+ CUDA cudart runtime]
	Archive string   // "zip" | "tar.gz"
}

// LlamaCandidates returns the ordered prebuilt llama.cpp archives to try for this
// hardware, best backend first, always ending in a CPU fallback that exists.
// NOTE: there is no prebuilt Linux CUDA archive (source build only).
func LlamaCandidates(hw platform.Hardware) []LlamaAsset {
	tag := latestTag(llamaRepo, llamaFallbackTag)
	base := "https://github.com/" + llamaRepo + "/releases/download/" + tag + "/"
	mk := func(backend, archive string, files ...string) LlamaAsset {
		urls := make([]string, len(files))
		for i, f := range files {
			urls[i] = base + f
		}
		return LlamaAsset{Backend: backend, URLs: urls, Archive: archive}
	}
	var out []LlamaAsset
	switch hw.OS {
	case "windows":
		if hw.GPUVendor == "nvidia" {
			out = append(out,
				mk("cuda", "zip", "llama-"+tag+"-bin-win-cuda-13.3-x64.zip", "cudart-llama-bin-win-cuda-13.3-x64.zip"),
				mk("cuda", "zip", "llama-"+tag+"-bin-win-cuda-12.4-x64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip"),
			)
		}
		if hw.GPUVendor != "none" && hw.GPUVendor != "" {
			out = append(out, mk("vulkan", "zip", "llama-"+tag+"-bin-win-vulkan-x64.zip"))
		}
		out = append(out, mk("cpu", "zip", "llama-"+tag+"-bin-win-cpu-x64.zip"))
	case "linux":
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

// SwapAsset returns the prebuilt llama-swap archive URL for this hardware.
func SwapAsset(hw platform.Hardware) (url, archive string, ok bool) {
	ver := strings.TrimPrefix(latestTag(swapRepo, swapFallbackTag), "v")
	var osName, arch, ext string
	switch hw.OS {
	case "windows":
		osName, arch, ext = "windows", "amd64", "zip"
	case "linux":
		osName, arch, ext = "linux", goArch(hw.Arch), "tar.gz"
	case "darwin":
		osName, arch, ext = "darwin", goArch(hw.Arch), "tar.gz"
	default:
		return "", "", false
	}
	file := fmt.Sprintf("llama-swap_%s_%s_%s.%s", ver, osName, arch, ext)
	url = fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", swapRepo, ver, file)
	return url, ext, true
}

func goArch(a string) string {
	if a == "arm64" {
		return "arm64"
	}
	return "amd64"
}
