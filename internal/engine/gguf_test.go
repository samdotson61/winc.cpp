package engine

import (
	"strings"
	"testing"
)

// The Qwen3.5 chat template's "System message must be at the beginning" guard breaks
// llama.cpp's tool-call parser generation. The patch must remove ONLY that guard and
// keep every other raise_exception intact.
func TestSystemFirstRaisePatch(t *testing.T) {
	tmpl := `{%- if message.role == "system" %}
    {%- if not loop.first %}
        {{- raise_exception('System message must be at the beginning.') }}
    {%- endif %}
{%- endif %}
{{- raise_exception('Unexpected message role.') }}`

	if !systemFirstRaise.MatchString(tmpl) {
		t.Fatal("guard should be detected")
	}
	patched := systemFirstRaise.ReplaceAllString(tmpl, "")
	if strings.Contains(patched, "must be at the beginning") {
		t.Error("the system-position guard must be removed")
	}
	if !strings.Contains(patched, "Unexpected message role") {
		t.Error("other raise_exception guards must be preserved")
	}

	// A template without the guard is left untouched (no override args).
	if systemFirstRaise.MatchString(`{{- raise_exception('No messages provided.') }}`) {
		t.Error("only the 'beginning' guard should match")
	}
}
