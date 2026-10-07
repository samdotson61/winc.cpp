package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeOpenClaw records every CLI call and answers from a tiny config store, so
// the whole register/restore flow runs without OpenClaw installed.
type fakeOpenClaw struct {
	calls []string
	store map[string]string // dot path -> value (JSON or plain)
}

func (f *fakeOpenClaw) run(_ string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 3 && args[0] == "config" && args[1] == "get":
		v, ok := f.store[args[2]]
		if !ok {
			return "Config path not found: " + args[2], errors.New("exit status 1")
		}
		return "banner\n" + v + "\n", nil
	case len(args) >= 4 && args[0] == "config" && args[1] == "set":
		val := args[3]
		if val == "--strict-json" && len(args) >= 5 {
			val = args[4]
		}
		f.store[args[2]] = val
		return "Updated " + args[2] + ". Restart the gateway to apply.", nil
	case len(args) >= 3 && args[0] == "config" && args[1] == "unset":
		delete(f.store, args[2])
		return "Removed " + args[2], nil
	}
	return "", errors.New("unexpected: " + strings.Join(args, " "))
}

// installFakeOpenClaw puts a fake openclaw launcher on PATH (so Resolve finds
// it) and routes the CLI through the recorder.
func installFakeOpenClaw(t *testing.T) *fakeOpenClaw {
	t.Helper()
	dir := t.TempDir()
	name := "openclaw"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	f := &fakeOpenClaw{store: map[string]string{}}
	old := openClawExec
	openClawExec = f.run
	t.Cleanup(func() { openClawExec = old })
	return f
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Fresh OpenClaw: the provider is written, the default model becomes
// winc/<alias>, and restore removes it again (there was none before).
func TestRegisterOpenClawModelFreshAndRestore(t *testing.T) {
	f := installFakeOpenClaw(t)
	restore := registerOpenClawModel("http://127.0.0.1:8080", "qwen3.5-4b", 32768, 4096)
	prov := f.store["models.providers.winc"]
	if !strings.Contains(prov, `"baseUrl":"http://127.0.0.1:8080"`) || !strings.Contains(prov, `"api":"anthropic-messages"`) ||
		!strings.Contains(prov, `"id":"qwen3.5-4b"`) || !strings.Contains(prov, `"contextWindow":32768`) || !strings.Contains(prov, `"maxTokens":4096`) {
		t.Fatalf("provider not written as expected: %s", prov)
	}
	if f.store["agents.defaults.model.primary"] != "winc/qwen3.5-4b" {
		t.Fatalf("default model = %q", f.store["agents.defaults.model.primary"])
	}
	restore()
	if _, ok := f.store["agents.defaults.model.primary"]; ok {
		t.Error("restore must unset a default that did not exist before")
	}
	restore() // second call is a no-op
	if count(f.calls, "config unset") != 1 {
		t.Errorf("restore must be idempotent: %v", f.calls)
	}
}

// A user with their own default model gets it back; an unchanged provider is
// not rewritten on the next launch.
func TestRegisterOpenClawModelRestoresPreviousAndSkipsUnchanged(t *testing.T) {
	f := installFakeOpenClaw(t)
	f.store["agents.defaults.model.primary"] = "ollama/mygemma"
	restore := registerOpenClawModel("http://127.0.0.1:8080", "qwen3.5-4b", 32768, 4096)
	if f.store["agents.defaults.model.primary"] != "winc/qwen3.5-4b" {
		t.Fatalf("default not switched: %q", f.store["agents.defaults.model.primary"])
	}
	restore()
	if f.store["agents.defaults.model.primary"] != "ollama/mygemma" {
		t.Fatalf("previous default not restored: %q", f.store["agents.defaults.model.primary"])
	}
	before := count(f.calls, "config set models.providers.winc")
	// Second launch, same endpoint + model: provider untouched (apiKey comes back redacted).
	f.store["models.providers.winc"] = strings.Replace(f.store["models.providers.winc"], `"winc-local"`, `"__OPENCLAW_REDACTED__"`, 1)
	restore2 := registerOpenClawModel("http://127.0.0.1:8080", "qwen3.5-4b", 32768, 4096)
	if count(f.calls, "config set models.providers.winc") != before {
		t.Errorf("unchanged provider must not be rewritten: %v", f.calls)
	}
	restore2()
	// A different window (or port) IS a change.
	registerOpenClawModel("http://127.0.0.1:8080", "qwen3.5-4b", 65536, 4096)()
	if count(f.calls, "config set models.providers.winc") != before+1 {
		t.Errorf("changed context window must rewrite the provider: %v", f.calls)
	}
}

// Without openclaw installed nothing is attempted and restore is a no-op.
func TestRegisterOpenClawModelMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", home)
	t.Setenv("LOCALAPPDATA", home)
	f := &fakeOpenClaw{store: map[string]string{}}
	old := openClawExec
	openClawExec = f.run
	defer func() { openClawExec = old }()
	registerOpenClawModel("http://127.0.0.1:8080", "m", 1, 1)()
	if len(f.calls) != 0 {
		t.Errorf("no openclaw -> no calls, got %v", f.calls)
	}
}

func TestOpenClawProviderMatches(t *testing.T) {
	want := openClawProviderJSON("http://127.0.0.1:8080", "qwen3.5-4b", 32768, 4096)
	shown := `banner
{
  "baseUrl": "http://127.0.0.1:8080",
  "apiKey": "__OPENCLAW_REDACTED__",
  "api": "anthropic-messages",
  "models": [{"id": "qwen3.5-4b", "name": "qwen3.5-4b", "reasoning": false, "input": ["text"], "contextWindow": 32768, "maxTokens": 4096, "cost": {"input": 0}, "api": "anthropic-messages"}]
}`
	if !openClawProviderMatches(shown, want) {
		t.Error("redacted key + defaulted fields must still match")
	}
	if openClawProviderMatches(strings.Replace(shown, "8080", "8099", 1), want) {
		t.Error("different port must not match")
	}
	if openClawProviderMatches(strings.Replace(shown, `"contextWindow": 32768`, `"contextWindow": 16384`, 1), want) {
		t.Error("different context window must not match")
	}
	if openClawProviderMatches("Config path not found: models.providers.winc", want) {
		t.Error("missing provider must not match")
	}
}
