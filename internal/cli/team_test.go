package cli

import (
	"testing"

	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/platform"
)

func TestWantTeam(t *testing.T) {
	cfg := config.Defaults() // mode = "auto"
	cat := &catalog.Catalog{Models: []catalog.Model{
		{Alias: "big", Tier: "mid", Size: "13 GB"},
		{Alias: "tiny", Tier: "nano", Size: "1 GB"},
	}}
	roomy := platform.Hardware{RAMMB: 32000, VRAMMB: 16000}

	// Explicit flags win.
	if wantTeam("claude", false, true, &cfg, cat, roomy, "big") {
		t.Error("--noteam must force single mode")
	}
	if !wantTeam("claude", true, false, &cfg, cat, roomy, "tiny") {
		t.Error("--team must force team even for a small main model")
	}

	// auto: team for a >=8 GB main model with RAM to spare; single for a smaller one.
	if !wantTeam("claude", false, false, &cfg, cat, roomy, "big") {
		t.Error("auto should engage team for a >=8 GB main model (team is the default)")
	}
	if wantTeam("claude", false, false, &cfg, cat, roomy, "tiny") {
		t.Error("auto should not team-ify a sub-8 GB main model")
	}

	// Not enough RAM for the workers -> no auto-team, even for a big model.
	tight := platform.Hardware{RAMMB: 4096, VRAMMB: 16000}
	if wantTeam("claude", false, false, &cfg, cat, tight, "big") {
		t.Error("auto should not team-ify when there isn't RAM for the workers")
	}

	// Team's tier env is Claude Code-specific.
	if wantTeam("opencode", false, false, &cfg, cat, roomy, "big") {
		t.Error("team should not auto-engage for non-claude apps")
	}

	// mode off / on override the size+RAM check.
	cfg.Team.Mode = "off"
	if wantTeam("claude", false, false, &cfg, cat, roomy, "big") {
		t.Error("mode=off must disable team")
	}
	cfg.Team.Mode = "on"
	if !wantTeam("claude", false, false, &cfg, cat, tight, "tiny") {
		t.Error("mode=on must always engage team for claude (overrides size/RAM)")
	}
}

func TestMidEnabled(t *testing.T) {
	for _, s := range []string{"qwen3.5-2b", "Qwen3.5-2B", "  qwen3.5-2b  "} {
		if !midEnabled(s) {
			t.Errorf("midEnabled(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "off", "none", "false", "  OFF "} {
		if midEnabled(s) {
			t.Errorf("midEnabled(%q) = true, want false", s)
		}
	}
}
