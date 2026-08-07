package cli

import (
	"os"
	"strings"
	"testing"
)

// The Version literal is what a plain `go build` reports, and it only gets
// touched at releases -- which is exactly how it sat at "1.31.0" through three
// of them. This pins it to the newest CHANGELOG heading so the gate fails the
// release commit that forgets the bump. Works on both branches: master headings
// are "## vX.Y.Z — date", winc-jobdar's newest is "## X.Y.Z-jobdar.N — date
// (winc-jobdar branch)", and each branch's literal must match its own.
func TestVersionMatchesChangelog(t *testing.T) {
	data, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		head := strings.TrimPrefix(strings.Fields(strings.TrimPrefix(line, "## "))[0], "v")
		if head != Version {
			t.Fatalf("version.go Version = %q but the newest CHANGELOG heading is %q -- bump the literal in the release commit", Version, head)
		}
		return
	}
	t.Fatal("no '## ' release heading found in CHANGELOG.md")
}

// versionBehindTag must not advertise a release to builds that already contain
// it: describe-suffixed clone builds and jobdar derivatives of the same tag.
func TestVersionBehindTag(t *testing.T) {
	for _, c := range []struct {
		v, tag string
		behind bool
	}{
		{"1.34.0", "v1.35.0", true},             // genuinely old
		{"1.35.0", "v1.35.0", false},            // exact release
		{"1.35.0-1-gabc1234", "v1.35.0", false}, // one commit PAST the tag
		{"1.35.0-jobdar.1", "v1.35.0", false},   // branch derivative of the release
		{"1.34.0-5-gdef", "v1.35.0", true},      // describe of an OLDER tag
		{"1.35.1", "v1.35.0", false},            // release literal ahead of a not-yet-published tag
		{"1.36.0-2-gaaa", "v1.35.1", false},     // describe past a NEWER base
		{"1.9.0", "v1.10.0", true},              // numeric, not lexicographic (9 < 10)
	} {
		if got := versionBehindTag(c.v, c.tag); got != c.behind {
			t.Errorf("versionBehindTag(%q, %q) = %v, want %v", c.v, c.tag, got, c.behind)
		}
	}
}
