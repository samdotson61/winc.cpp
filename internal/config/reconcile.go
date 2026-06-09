package config

import (
	"os"
	"regexp"
	"strings"

	"winc/internal/paths"
)

// sectionHeader matches a TOML table header line like "[team]" or "[reasoning.adaptive]",
// tolerating a trailing inline comment. It deliberately does NOT match "[[custom_models]]"
// (array-of-tables) or commented lines.
var sectionHeader = regexp.MustCompile(`^\[([^\[\]]+)\]\s*(#.*)?$`)

type tomlSection struct {
	name string // "" for the preamble before the first header
	text string // the header line + everything up to (not including) the next header
}

func splitSections(s string) []tomlSection {
	var out []tomlSection
	cur := tomlSection{}
	for _, line := range strings.Split(s, "\n") {
		if m := sectionHeader.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			if cur.name != "" || cur.text != "" {
				out = append(out, cur)
			}
			cur = tomlSection{name: m[1], text: line + "\n"}
		} else {
			cur.text += line + "\n"
		}
	}
	if cur.name != "" || cur.text != "" {
		out = append(out, cur)
	}
	return out
}

// SyncMissingSections appends to winc.toml any top-level sections that exist in the
// current default template but are absent from the user's file (e.g. a pre-[team] config
// gains the [team] block after an update). It only APPENDS whole missing sections -- it
// never edits or removes existing content, so user edits and comments are preserved. Keys
// missing inside a section the user already has keep working via backfill (just not shown).
// Returns the names of the sections added.
func SyncMissingSections() ([]string, error) {
	p := paths.ConfigPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, s := range splitSections(string(data)) {
		if s.name != "" {
			have[s.name] = true
		}
	}
	var added []string
	var b strings.Builder
	b.WriteString(string(data))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	for _, s := range splitSections(defaultTOML) {
		if s.name == "" || have[s.name] {
			continue
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(s.text, "\n") + "\n")
		added = append(added, s.name)
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, os.WriteFile(p, []byte(b.String()), 0o600)
}
