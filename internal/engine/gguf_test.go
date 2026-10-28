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

// A template that opens the thinking turn unconditionally with no
// enable_thinking switch (LFM2.5) gets the switch; templates that already have
// one (Qwen3) or open nothing are left alone.
func TestForcedThinkOpenPatch(t *testing.T) {
	lfm := "{%- for m in messages -%}{{ m.content }}{%- endfor -%}\n{%- if add_generation_prompt -%}\n    " + forcedThinkOpen + "\n{%- endif -%}\n"
	out, changed := patchChatTemplate(lfm)
	if !changed {
		t.Fatal("the forced-open thinking turn must be patched")
	}
	if strings.Contains(out, forcedThinkOpen+"\n{%- endif") || !strings.Contains(out, "enable_thinking is defined and not enable_thinking") {
		t.Fatalf("patch must make the opener conditional, got:\n%s", out)
	}
	if !strings.Contains(out, `<think>\n\n</think>\n`) {
		t.Fatalf("the no-think branch must close an empty thinking block, got:\n%s", out)
	}
	if strings.Count(out, "<|im_start|>assistant") != 2 {
		t.Fatalf("exactly the two branches must emit the assistant turn, got:\n%s", out)
	}
	// Qwen3-style: already switchable -> untouched even though it opens <think>.
	qwen := `{%- if add_generation_prompt %}{{- '<|im_start|>assistant\n' }}{%- if enable_thinking is defined and enable_thinking is false %}{{- '<think>\n\n</think>\n\n' }}{%- endif %}{%- endif %}`
	if _, changed := patchChatTemplate(qwen); changed {
		t.Error("a template with its own enable_thinking switch must not be patched")
	}
	// No thinking opener at all -> untouched.
	if _, changed := patchChatTemplate(`{%- if add_generation_prompt -%}{{- "<|im_start|>assistant\n" -}}{%- endif -%}`); changed {
		t.Error("a template that opens no thinking turn must not be patched")
	}
	// Both fixes compose: the Qwen3.5 guard AND a forced opener.
	both := "{{- raise_exception('System message must be at the beginning.') }}" + lfm
	out, changed = patchChatTemplate(both)
	if !changed || strings.Contains(out, "beginning") || !strings.Contains(out, "enable_thinking") {
		t.Fatalf("both fixes must apply, got:\n%s", out)
	}
}
