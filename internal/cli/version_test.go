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
