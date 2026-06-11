package cli

import (
	"os"
	"path/filepath"
	"testing"

	"winc/internal/agent"
	"winc/internal/catalog"
	"winc/internal/config"
	"winc/internal/platform"
)

// Workers claim GPU space only when their full footprint (weights + KV + compute
// buffer) is known and fits; an unknown model size must never claim the GPU.
func TestWorkerGPUNeedMB(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "worker.gguf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, want := workerGPUNeedMB(p, 65536), 100+65536/64+512; got != want {
		t.Errorf("need = %d, want %d", got, want)
	}
	if got := workerGPUNeedMB(filepath.Join(dir, "missing.gguf"), 65536); got < 1<<29 {
		t.Errorf("unknown model size must never claim GPU, got %d", got)
	}
}

func TestWantTeam(t *testing.T) {
	cfg := config.Defaults() // mode = "auto"
	cat := &catalog.Catalog{Models: []catalog.Model{
		{Alias: "big", Tier: "mid", Size: "13 GB"},
		{Alias: "smallm", Tier: "small", Size: "6 GB"},
		{Alias: "tiny", Tier: "nano", Size: "1 GB"},
	}}
	roomy := platform.Hardware{RAMMB: 32000, VRAMMB: 28000} // above the 16 GB discrete class

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

	// Hardware class gate: at or below 16 GB discrete (or 24 GB unified, or
	// CPU-only) the head model alone is the right load -- auto never teams,
	// no matter how much system RAM is free. Explicit --team still forces.
	gpu16 := platform.Hardware{RAMMB: 64000, VRAMMB: 16303}
	if wantTeam("claude", false, false, &cfg, cat, gpu16, "big") {
		t.Error("auto must not team on a 16 GB card")
	}
	if !wantTeam("claude", true, false, &cfg, cat, gpu16, "big") {
		t.Error("--team must still force team on a 16 GB card")
	}
	mac24 := platform.Hardware{RAMMB: 24576, Unified: true, GPUVendor: "apple", VRAMMB: 0}
	if wantTeam("claude", false, false, &cfg, cat, mac24, "big") {
		t.Error("auto must not team on a 24 GB unified Mac")
	}
	mac32 := platform.Hardware{RAMMB: 32768, Unified: true, GPUVendor: "apple", VRAMMB: 0}
	if !wantTeam("claude", false, false, &cfg, cat, mac32, "big") {
		t.Error("auto should team on a 32 GB unified Mac")
	}
	cpuOnly := platform.Hardware{RAMMB: 32000, VRAMMB: 0}
	if wantTeam("claude", false, false, &cfg, cat, cpuOnly, "big") {
		t.Error("auto must never team on a CPU-only box")
	}

	// Not even the smallest worker fits -> fall back to a single model.
	tight := platform.Hardware{RAMMB: 4096, VRAMMB: 28000}
	if wantTeam("claude", false, false, &cfg, cat, tight, "big") {
		t.Error("auto should fall back to single only when not even the smallest worker fits")
	}
	// Room for the smallest worker (even if not the whole set) -> still team.
	moderate := platform.Hardware{RAMMB: 8000, VRAMMB: 28000}
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

func TestBuildDispatch(t *testing.T) {
	cfg := config.Defaults()
	slots := agent.Slots{Opus: "main-m", Sonnet: "son-4b", Haiku: "hk-0.8b"}

	// dynamic, every worker up + main escalation: 4 ascending rungs, last is catch-all.
	d := buildDispatch(&cfg, "dynamic", slots, "http://s", "http://h", "http://m", "http://main", "son-4b", "hk-0.8b", true)
	if d.ladderTag != "hk-0.8b" || d.subagentModel != "hk-0.8b" {
		t.Errorf("dynamic should tag subagents with the haiku alias, got tag=%q model=%q", d.ladderTag, d.subagentModel)
	}
	if len(d.routes) != 0 || len(d.ladder) != 4 {
		t.Fatalf("dynamic full house should be ladder-only with 4 rungs, got %d routes / %d rungs", len(d.routes), len(d.ladder))
	}
	for i, n := range []string{"haiku", "mid", "sonnet", "escalated"} {
		if d.ladder[i].Name != n {
			t.Errorf("rung %d name = %q, want %q", i, d.ladder[i].Name, n)
		}
	}
	for i := 0; i < len(d.ladder)-1; i++ {
		if d.ladder[i].MaxEstTokens >= d.ladder[i+1].MaxEstTokens {
			t.Errorf("thresholds must ascend: rung %d (%d) >= rung %d (%d)", i, d.ladder[i].MaxEstTokens, i+1, d.ladder[i+1].MaxEstTokens)
		}
	}
	last := d.ladder[len(d.ladder)-1]
	if last.MaxEstTokens != 1<<30 || last.Tools != nil || last.MaxTokens != 0 {
		t.Errorf("the escalated (main) rung must be the uncapped catch-all with all tools, got %+v", last)
	}

	// dynamic with only the haiku worker: a single catch-all rung.
	d = buildDispatch(&cfg, "dynamic", slots, "", "http://h", "", "http://main", "son-4b", "hk-0.8b", false)
	if len(d.ladder) != 1 || d.ladder[0].Name != "haiku" || d.ladder[0].MaxEstTokens != 1<<30 {
		t.Fatalf("haiku-only dynamic should be one catch-all rung, got %+v", d.ladder)
	}

	// tiered: per-agent pins, no ladder.
	d = buildDispatch(&cfg, "tiered", slots, "http://s", "http://h", "", "http://main", "son-4b", "hk-0.8b", false)
	if len(d.ladder) != 0 || d.ladderTag != "" || len(d.routes) != 2 {
		t.Fatalf("tiered should be routes-only, got %d routes, tag %q", len(d.routes), d.ladderTag)
	}
	if d.routes[0].Name != "sonnet" || d.routes[0].Model != "son-4b" || d.routes[1].Name != "haiku" || d.routes[1].Model != "hk-0.8b" {
		t.Errorf("tiered routes mis-built: %+v", d.routes)
	}

	// forced sonnet with its worker missing: tag still set (requests just fall back to main).
	d = buildDispatch(&cfg, "sonnet", slots, "", "", "", "http://main", "son-4b", "hk-0.8b", false)
	if d.ladderTag != "son-4b" || len(d.ladder) != 0 {
		t.Errorf("sonnet-forced with no worker: want tag son-4b + empty ladder, got tag=%q rungs=%d", d.ladderTag, len(d.ladder))
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
