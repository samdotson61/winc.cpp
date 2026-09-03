package engine

import "testing"

// llama-server's --version line changed shape with upstream's versioned
// releases (0.2.0, 2026-08-21): the build number moved from the leading
// digits to a "(build N, ...)" clause. Both forms must yield the bNNNN tag,
// and the new form must never read as "b0" (the leading "0.3.0" digits).
func TestLlamaBuildTag(t *testing.T) {
	cases := map[string]string{
		"version: 9651 (e3bb1add8)\nbuilt with AppleClang":                          "b9651",
		"version: 0.3.0-dev (build 10790, commit 8c1a25166)\nbuilt with AppleClang": "b10790",
		"version: 0.3.0 (build 10621, commit c1d0e7a00)":                            "b10621",
		"garbage": "",
		"":        "",
	}
	for in, want := range cases {
		if got := llamaBuildTag([]byte(in)); got != want {
			t.Errorf("llamaBuildTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBuildTag(t *testing.T) {
	for tag, want := range map[string]bool{"b10790": true, "b1": true, "v0.3.0": false, "b": false, "": false, "b10a": false} {
		if got := isBuildTag(tag); got != want {
			t.Errorf("isBuildTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestParseNightlyTag(t *testing.T) {
	for in, want := range map[string]string{"b10621\n": "b10621", "  b10621 extra\n": "b10621", "": ""} {
		if got := parseNightlyTag([]byte(in)); got != want {
			t.Errorf("parseNightlyTag(%q) = %q, want %q", in, got, want)
		}
	}
}
