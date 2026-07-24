package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeContextGGUF builds a minimal GGUF carrying "<arch>.context_length", always
// preceded by a rope "original_context_length" decoy that a careless suffix match
// would pick up instead.
func writeContextGGUF(t *testing.T, name string, trained int) string {
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
	nkv := uint64(1)
	if trained > 0 {
		nkv = 2
	}
	w(uint32(ggufMagic))
	w(uint32(3)) // version
	w(uint64(0)) // n_tensors
	w(nkv)
	str("testarch.rope.scaling.original_context_length")
	w(uint32(4))
	w(uint32(4096))
	if trained > 0 {
		str("testarch.context_length")
		w(uint32(4))
		w(uint32(trained))
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTrainedContext(t *testing.T) {
	p := writeContextGGUF(t, "trained.gguf", 128000)
	if got := TrainedContext(p); got != 128000 {
		t.Errorf("TrainedContext = %d, want 128000 (the rope decoy must not win)", got)
	}
	// No context_length key -> 0 "unknown", which must not be read as a real cap.
	none := writeContextGGUF(t, "none.gguf", 0)
	if got := TrainedContext(none); got != 0 {
		t.Errorf("TrainedContext(no key) = %d, want 0", got)
	}
	if got := TrainedContext(filepath.Join(t.TempDir(), "missing.gguf")); got != 0 {
		t.Errorf("TrainedContext(missing) = %d, want 0", got)
	}
}

// The reporting bug: winc sized, printed and estimated decode for 262144 on a
// model llama-server then capped to 128000, so every one of those numbers
// described a configuration that never ran.
func TestClampToTrainedContext(t *testing.T) {
	p := writeContextGGUF(t, "lfm-like.gguf", 128000)
	if got := clampToTrainedContext(p, 262144, false); got != 128000 {
		t.Errorf("clamp(262144) = %d, want 128000", got)
	}
	// At or under the trained ceiling nothing changes.
	if got := clampToTrainedContext(p, 65536, false); got != 65536 {
		t.Errorf("clamp(65536) = %d, want 65536", got)
	}
	if got := clampToTrainedContext(p, 128000, false); got != 128000 {
		t.Errorf("clamp(128000) = %d, want 128000", got)
	}
	// Unknown trained length must leave the target alone rather than invent a cap.
	unknown := writeContextGGUF(t, "unknown.gguf", 0)
	if got := clampToTrainedContext(unknown, 262144, false); got != 262144 {
		t.Errorf("clamp with unknown trained length = %d, want 262144 untouched", got)
	}
	// A pin is honoured exactly up to the ceiling, and clamped past it (the engine
	// caps there regardless) -- the pinned path warns, which is covered separately.
	if got := clampToTrainedContext(p, 262144, true); got != 128000 {
		t.Errorf("pinned clamp = %d, want 128000", got)
	}
}
