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
