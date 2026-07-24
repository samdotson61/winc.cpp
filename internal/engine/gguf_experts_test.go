package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeExpertGGUF builds a minimal GGUF carrying an optional "<arch>.expert_count"
// (and always an expert_used_count, to prove the scan doesn't confuse the two),
// saved under a caller-chosen filename so a test can pit metadata against the name.
func writeExpertGGUF(t *testing.T, name string, experts int) string {
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
	nkv := uint64(2)
	if experts > 0 {
		nkv = 3
	}
	w(uint32(ggufMagic))
	w(uint32(3)) // version
	w(uint64(0)) // n_tensors
	w(nkv)
	// A decoy that must NOT satisfy the ".expert_count" suffix match.
	str("test.expert_used_count")
	w(uint32(4))
	w(uint32(4))
	str("general.architecture")
	w(uint32(8))
	str("testarch")
	if experts > 0 {
		str("testarch.expert_count")
		w(uint32(4))
		w(uint32(experts))
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The regression this guards: a MoE whose filename carries no active-param tag.
// Mixtral-8x7B, DeepSeek-V2-Lite and grok-1 are all real MoEs the name heuristic
// reads as dense -- which meant winc never offloaded their experts, leaving a
// VRAM-tight machine with a floor-level context or an OOM.
func TestIsMoEMetadataBeatsFilename(t *testing.T) {
	p := writeExpertGGUF(t, "Mixtral-8x7B-Instruct-v0.1-Q4_K_M.gguf", 8)
	if isMoEFile(p) {
		t.Fatal("precondition: this filename should fool the name heuristic")
	}
	if got := ExpertCount(p); got != 8 {
		t.Errorf("ExpertCount = %d, want 8 (expert_used_count must not be mistaken for it)", got)
	}
	if !IsMoE(p) {
		t.Error("a model whose metadata declares 8 experts must be detected as MoE")
	}
}

// Metadata read cleanly and says dense -> that beats a MoE-looking name, so a
// dense model misnamed with an active-param tag is not sent down the MoE path.
func TestIsMoEDenseMetadataBeatsMoEName(t *testing.T) {
	p := writeExpertGGUF(t, "Totally-Not-A3B-Q4_K_M.gguf", 0)
	if !isMoEFile(p) {
		t.Fatal("precondition: this filename should look like MoE to the heuristic")
	}
	if got := ExpertCount(p); got != 0 {
		t.Errorf("ExpertCount = %d, want 0 for a dense model", got)
	}
	if IsMoE(p) {
		t.Error("metadata says dense; that must win over the filename")
	}
}

// No readable file (a catalogue entry naming a model that isn't downloaded yet)
// -> ExpertCount is -1 "unknown" and detection falls back to the name convention.
func TestIsMoEFallsBackWhenMetadataUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Qwen3.6-35B-A3B-UD-IQ3_S.gguf")
	if got := ExpertCount(missing); got != -1 {
		t.Errorf("ExpertCount(missing) = %d, want -1 (unknown, not 0/dense)", got)
	}
	if !IsMoE(missing) {
		t.Error("with metadata unavailable, the MoE name convention must still apply")
	}
	denseMissing := filepath.Join(t.TempDir(), "Qwen3.6-27B-Q3_K_M.gguf")
	if IsMoE(denseMissing) {
		t.Error("unreadable + dense name should stay dense")
	}
}
