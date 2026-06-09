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
		{Alias: "smallm", Tier: "small", Size: "6 GB"},
		{Alias: "tiny", Tier: "nano", Size: "1 GB"},
	}}
	roomy := platform.Hardware{RAMMB: 32000, VRAMMB: 16000}

	// Explicit flags win.
	if wantTeam("claude", false, true, &cfg, cat, roomy, "big") {
		t.Error("--noteam must force single mode")
	}
	if !wantTeam("claude", true, false, &cfg, cat, roomy, "tiny") {
		t.Error("--team must force team even for a nano main model")
	}

	// auto: team for anything ABOVE the nano tier with RAM to spare; nano stays single.
	if !wantTeam("claude", false, false, &cfg, cat, roomy, "big") {
		t.Error("auto should engage team for a mid main model")
	}
	if !wantTeam("claude", false, false, &cfg, cat, roomy, "smallm") {
		t.Error("auto should engage team for a small (above-nano) main model")
	}
	if wantTeam("claude", false, false, &cfg, cat, roomy, "tiny") {
		t.Error("auto should not team-ify a nano main model")
	}

	// Not even the smallest worker fits -> fall back to a single model.
	tight := platform.Hardware{RAMMB: 4096, VRAMMB: 16000}
	if wantTeam("claude", false, false, &cfg, cat, tight, "big") {
		t.Error("auto should fall back to single only when not even the smallest worker fits")
	}
	// Room for the smallest worker (even if not the whole set) -> still team.
	moderate := platform.Hardware{RAMMB: 8000, VRAMMB: 16000}
	if !wantTeam("claude", false, false, &cfg, cat, moderate, "big") {
		t.Error("auto should team (with whatever workers fit) once the smallest worker fits")
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

func TestSmallRAM(t *testing.T) {
	cases := []struct {
		name  string
		ramMB int
		want  bool
	}{
		{"unknown RAM is not small", 0, false},
		{"8GB", 8192, true},
		{"16GB reported under (windows carve-outs)", 16303, true},
		{"16GB exact (mac unified)", 16384, true},
		{"threshold edge", smallRAMMB, true},
		{"just above threshold", smallRAMMB + 1, false},
		{"24GB", 24576, false},
		{"32GB", 32768, false},
	}
	for _, c := range cases {
		if got := smallRAM(platform.Hardware{RAMMB: c.ramMB}); got != c.want {
			t.Errorf("%s: smallRAM(%d MB) = %v, want %v", c.name, c.ramMB, got, c.want)
		}
	}
}

func TestWorkerGeometry(t *testing.T) {
	big := platform.Hardware{RAMMB: 32768}
	small := platform.Hardware{RAMMB: 16303}
	cases := []struct {
		name                                       string
		cfgPar                                     int
		hw                                         platform.Hardware
		workerPar, workerCtx, sonnetPar, sonnetCtx int
	}{
		{"defaults on a big system", 0, big, 4, 32768, 2, 32768},
		{"small system halves fan-out, keeps the window", 0, small, 2, 32768, 1, 32768},
		{"explicit parallel on a big system", 8, big, 8, 65536, 2, 32768},
		{"explicit parallel halves on a small system", 8, small, 4, 65536, 1, 32768},
		{"odd parallel rounds up", 3, small, 2, 24576, 1, 32768},
		{"parallel floor of 1", 1, small, 1, 8192, 1, 32768},
		{"unknown RAM treated as big", 0, platform.Hardware{}, 4, 32768, 2, 32768},
	}
	for _, c := range cases {
		wp, wc, sp, sc := workerGeometry(c.cfgPar, c.hw)
		if wp != c.workerPar || wc != c.workerCtx || sp != c.sonnetPar || sc != c.sonnetCtx {
			t.Errorf("%s: workerGeometry(%d) = (%d, %d, %d, %d), want (%d, %d, %d, %d)",
				c.name, c.cfgPar, wp, wc, sp, sc, c.workerPar, c.workerCtx, c.sonnetPar, c.sonnetCtx)
		}
	}

	// The whole point of the small-RAM reduction: per-slot context doubles.
	wp, wc, sp, sc := workerGeometry(0, small)
	if wc/wp != 16384 {
		t.Errorf("small-system research per-slot context = %d, want 16384 (double the 8192 default)", wc/wp)
	}
	if sc/sp != 32768 {
		t.Errorf("small-system collator per-slot context = %d, want 32768 (double the 16384 default)", sc/sp)
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
