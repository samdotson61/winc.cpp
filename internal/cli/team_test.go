package cli

import (
	"testing"

	"winc/internal/catalog"
	"winc/internal/config"
)

func TestWantTeam(t *testing.T) {
	cfg := config.Defaults() // mode = "auto"
	cat := &catalog.Catalog{Models: []catalog.Model{
		{Alias: "big", Tier: "mid"},
		{Alias: "tiny", Tier: "nano"},
	}}

	// Explicit flags win.
	if wantTeam("claude", false, true, &cfg, cat, "big") {
		t.Error("--noteam must force single mode")
	}
	if !wantTeam("claude", true, false, &cfg, cat, "tiny") {
		t.Error("--team must force team even for a small main model")
	}

	// auto (default): team for a big (mid+) main model, single for a nano one.
	if !wantTeam("claude", false, false, &cfg, cat, "big") {
		t.Error("auto should engage team for a mid+ main model (team is the default)")
	}
	if wantTeam("claude", false, false, &cfg, cat, "tiny") {
		t.Error("auto should not team-ify a nano main model")
	}

	// Team's tier env is Claude Code-specific.
	if wantTeam("opencode", false, false, &cfg, cat, "big") {
		t.Error("team should not auto-engage for non-claude apps")
	}

	// mode off / on override auto.
	cfg.Team.Mode = "off"
	if wantTeam("claude", false, false, &cfg, cat, "big") {
		t.Error("mode=off must disable team")
	}
	cfg.Team.Mode = "on"
	if !wantTeam("claude", false, false, &cfg, cat, "tiny") {
		t.Error("mode=on must always engage team for claude")
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
