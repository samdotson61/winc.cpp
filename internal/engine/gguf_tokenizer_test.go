package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeTokenizerGGUF builds a minimal GGUF carrying just the three tokenizer keys
// the fingerprint is made of, under a caller-chosen filename.
func writeTokenizerGGUF(t *testing.T, name, tokModel string, vocab, eos int) string {
	t.Helper()
	var b bytes.Buffer
	le := binary.LittleEndian
	w := func(v any) {
		if err := binary.Write(&b, le, v); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) {
		w(uint64(len(s)))
		b.WriteString(s)
	}
	w(uint32(ggufMagic))
	w(uint32(3)) // version
	w(uint64(0)) // n_tensors
	w(uint64(3)) // n_kv
	str("tokenizer.ggml.model")
	w(uint32(8))
	str(tokModel)
	str("tokenizer.ggml.tokens")
	w(uint32(9)) // array
	w(uint32(8)) // of strings
	w(uint64(vocab))
	for i := 0; i < vocab; i++ {
		str("t")
	}
	str("tokenizer.ggml.eos_token_id")
	w(uint32(4))
	w(uint32(eos))
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenizerFingerprint(t *testing.T) {
	a := writeTokenizerGGUF(t, "model-a.gguf", "gpt2", 32, 31)
	if got, want := TokenizerFingerprint(a), "gpt2:32:31"; got != want {
		t.Errorf("fingerprint = %q, want %q", got, want)
	}
	// Same vocabulary under a completely unrelated filename -> same fingerprint.
	// This is the case the old family-name regex could not see: a post-train
	// released under a new name still shares its base model's tokenizer.
	b := writeTokenizerGGUF(t, "Some-Post-Train-9B.gguf", "gpt2", 32, 31)
	if TokenizerFingerprint(a) != TokenizerFingerprint(b) {
		t.Error("same tokenizer under a different filename must fingerprint the same")
	}
	// A different vocabulary size must not collide.
	c := writeTokenizerGGUF(t, "model-c.gguf", "gpt2", 40, 31)
	if TokenizerFingerprint(a) == TokenizerFingerprint(c) {
		t.Error("different vocab sizes must not share a fingerprint")
	}
	// A different EOS must not collide either.
	d := writeTokenizerGGUF(t, "model-d.gguf", "gpt2", 32, 7)
	if TokenizerFingerprint(a) == TokenizerFingerprint(d) {
		t.Error("different eos ids must not share a fingerprint")
	}
	// Unreadable -> "" so callers can fall back rather than match on a lie.
	if got := TokenizerFingerprint(filepath.Join(t.TempDir(), "missing.gguf")); got != "" {
		t.Errorf("missing file fingerprint = %q, want \"\"", got)
	}
}

// An explicit draft_model reaches --spec-draft-model after only an existence
// check, so a wrong-vocabulary draft fails inside the engine with an error that
// names neither file. DraftMismatch is the guard, and it must only fire on a
// mismatch it can prove.
func TestDraftMismatch(t *testing.T) {
	target := writeTokenizerGGUF(t, "target.gguf", "gpt2", 32, 31)
	same := writeTokenizerGGUF(t, "draft-ok.gguf", "gpt2", 32, 31)
	other := writeTokenizerGGUF(t, "draft-bad.gguf", "gemma4", 64, 1)
	missing := filepath.Join(t.TempDir(), "nope.gguf")

	if DraftMismatch(target, same) {
		t.Error("identical tokenizers must not be reported as a mismatch")
	}
	if !DraftMismatch(target, other) {
		t.Error("different tokenizers must be reported as a mismatch")
	}
	// Unprovable -> not a mismatch, so the pair passes through to the engine.
	if DraftMismatch(target, missing) {
		t.Error("an unreadable draft must not be blocked on a guess")
	}
	if DraftMismatch(missing, target) {
		t.Error("an unreadable target must not be blocked on a guess")
	}
}
