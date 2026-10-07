package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The OpenClaw entry comparison must see through the CLI's banner and key
// order, and must NOT match a stale path (a moved winc re-registers). The
// registered command uses forward slashes (the JSON crosses cmd.exe + node
// argv parsing, which eat backslashes next to quotes).
func TestOpenClawEntryMatches(t *testing.T) {
	// A native path on every OS: FromSlash gives Windows builds C:\w\winc.exe
	// (which must register as C:/w/winc.exe) and leaves Unix builds' slashes
	// alone, where ToSlash is a no-op -- a hard-coded backslash input failed on
	// CI's Linux/macOS legs, and filepath.Join("C:", ...) is drive-relative on
	// Windows (C:w\...).
	want := openClawServerJSON(filepath.FromSlash("C:/w/winc.exe"))
	if !strings.Contains(want, `"command":"C:/w/winc.exe"`) {
		t.Fatalf("registered path must use forward slashes: %s", want)
	}
	shown := "banner line\nMCP server winc (C:\\Users\\x\\.openclaw\\openclaw.json):\n{\n  \"args\": [\"mcp-search\"],\n  \"command\": \"C:/w/winc.exe\"\n}\n"
	if !openClawEntryMatches(shown, want) {
		t.Errorf("same entry (reordered, with banner) must match:\n%s", shown)
	}
	nested := "MCP servers:\n{\"winc\": {\"command\": \"C:/w/winc.exe\", \"args\": [\"mcp-search\"]}}\n"
	if !openClawEntryMatches(nested, want) {
		t.Error("nested form must match")
	}
	if openClawEntryMatches(strings.Replace(shown, "C:/w/winc.exe", "C:/old/winc.exe", 1), want) {
		t.Error("a stale path must not match")
	}
	if openClawEntryMatches("No MCP server named \"winc\" in x.json.", want) {
		t.Error("no entry must not match")
	}
}
