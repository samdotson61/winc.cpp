package agent

import (
	"os"
	"strings"
	"testing"
)

// envVal returns the last value for key in a KEY=VALUE env slice (last wins, like the OS).
func envVal(env []string, key string) (string, bool) {
	p := key + "="
	val, ok := "", false
	for _, e := range env {
		if strings.HasPrefix(e, p) {
			val, ok = e[len(p):], true
		}
	}
	return val, ok
}

// The auto-compaction trigger must fire at the REAL window minus max(8k,
// window/8) of headroom. Claude Code 2.1.x refuses window values under 100k
// (verified against the binary: an out-of-range value is treated as unset and
// the agent believes the 100k default), so small real windows are reported AT
// the floor and the trigger is placed absolutely via the pct of the BELIEVED
// window. Windows >= 100k keep the original math exactly.
func TestEnvCompactionTrigger(t *testing.T) {
	cases := map[int]struct{ window, pct string }{
		65536:  {"100000", "57"}, // (65536-8192)/100000 -- the 27B's real window
		49152:  {"100000", "40"}, // (49152-8192)/100000
		131072: {"131072", "87"}, // >= floor: passed through, window/8 reserve
		200000: {"200000", "87"},
		16384:  {"100000", "10"}, // floor-rung window: pct clamps low, never high
		8192:   {"100000", "10"},
	}
	for win, want := range cases {
		env := Env("http://local", Slots{Sonnet: "m", Opus: "m", Haiku: "m"}, 0, win, "", "")
		if v, _ := envVal(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW"); v != want.window {
			t.Errorf("window %d: believed window = %q, want %q", win, v, want.window)
		}
		if v, _ := envVal(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"); v != want.pct {
			t.Errorf("window %d: trigger pct = %q, want %q", win, v, want.pct)
		}
	}
}

func TestEnvMapsTiersAndPinsMain(t *testing.T) {
	slots := Slots{Sonnet: "small-4b", Opus: "big-35b", Haiku: "tiny-0.8b"}
	env := Env("http://local", slots, 0, 0, "big-35b", "tiny-0.8b")
	if v, _ := envVal(env, "ANTHROPIC_DEFAULT_SONNET_MODEL"); v != "small-4b" {
		t.Errorf("sonnet tier = %q, want small-4b", v)
	}
	if v, _ := envVal(env, "ANTHROPIC_DEFAULT_HAIKU_MODEL"); v != "tiny-0.8b" {
		t.Errorf("haiku tier = %q, want tiny-0.8b", v)
	}
	if v, _ := envVal(env, "ANTHROPIC_DEFAULT_OPUS_MODEL"); v != "big-35b" {
		t.Errorf("opus tier = %q, want big-35b", v)
	}
	// The main agent must be pinned, or Claude Code would default it to the sonnet
	// tier (a small worker) instead of the launched model.
	if v, ok := envVal(env, "ANTHROPIC_MODEL"); !ok || v != "big-35b" {
		t.Errorf("ANTHROPIC_MODEL = %q ok=%v, want big-35b", v, ok)
	}
	// In team mode every subagent (Task + Workflow fan-out) is forced onto the worker.
	if v, ok := envVal(env, "CLAUDE_CODE_SUBAGENT_MODEL"); !ok || v != "tiny-0.8b" {
		t.Errorf("CLAUDE_CODE_SUBAGENT_MODEL = %q ok=%v, want tiny-0.8b", v, ok)
	}
}

func TestEnvSetsLocalTimeouts(t *testing.T) {
	os.Unsetenv("API_TIMEOUT_MS")
	os.Unsetenv("CLAUDE_STREAM_IDLE_TIMEOUT_MS")
	env := Env("http://local", Slots{Sonnet: "s", Opus: "o", Haiku: "h"}, 0, 0, "", "")
	// Generous timeouts so slow local prefill doesn't trip the stream-idle watchdog.
	if v, ok := envVal(env, "API_TIMEOUT_MS"); !ok || v == "" {
		t.Error("API_TIMEOUT_MS should be set for slow local models")
	}
	if v, ok := envVal(env, "CLAUDE_STREAM_IDLE_TIMEOUT_MS"); !ok || v == "" {
		t.Error("CLAUDE_STREAM_IDLE_TIMEOUT_MS should be set (the time-to-first-token watchdog)")
	}
	// A user-chosen value is respected, not overridden.
	t.Setenv("API_TIMEOUT_MS", "12345")
	env2 := Env("http://local", Slots{Opus: "o"}, 0, 0, "", "")
	if v, _ := envVal(env2, "API_TIMEOUT_MS"); v != "12345" {
		t.Errorf("user API_TIMEOUT_MS must be respected, got %q", v)
	}
}

func TestEnvNoPinWhenMainEmpty(t *testing.T) {
	os.Unsetenv("ANTHROPIC_MODEL")
	os.Unsetenv("CLAUDE_CODE_SUBAGENT_MODEL")
	env := Env("http://local", Slots{Sonnet: "s", Opus: "o", Haiku: "h"}, 0, 0, "", "")
	if _, ok := envVal(env, "ANTHROPIC_MODEL"); ok {
		t.Error("ANTHROPIC_MODEL must not be set in single/multi mode (empty mainModel)")
	}
	if _, ok := envVal(env, "CLAUDE_CODE_SUBAGENT_MODEL"); ok {
		t.Error("CLAUDE_CODE_SUBAGENT_MODEL must not be set when empty")
	}
}
