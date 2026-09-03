package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeMTPGGUF builds a minimal GGUF with (optionally) "<arch>.nextn_predict_layers".
func writeMTPGGUF(t *testing.T, name string, nextn int) string {
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
	if nextn > 0 {
		nkv = 2
	}
	w(uint32(ggufMagic))
	w(uint32(3))
	w(uint64(0))
	w(nkv)
	str("qwen35.block_count")
	w(uint32(4))
	w(uint32(65))
	if nextn > 0 {
		str("qwen35.nextn_predict_layers")
		w(uint32(4))
		w(uint32(nextn))
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Qwen3.8 bakes its MTP head into every standard quant with nothing in the
// filename: detection must come from metadata, and the head's block must be
// excluded from the main-block count (it only loads for speculation).
func TestBakedMTPFromMetadata(t *testing.T) {
	baked := writeMTPGGUF(t, "Qwen3.8-27B-UD-Q4_K_M.gguf", 1)
	plain := writeMTPGGUF(t, "Qwen3.6-27B-Q4_K_M.gguf", 0)
	if MTPLayers(baked) != 1 || !isMTPFile(baked) {
		t.Errorf("baked-head file should detect as MTP from metadata (layers=%d)", MTPLayers(baked))
	}
	if MTPLayers(plain) != 0 || isMTPFile(plain) {
		t.Errorf("plain file must not detect as MTP (layers=%d)", MTPLayers(plain))
	}
	if got := mainBlocks(baked); got != 64 {
		t.Errorf("mainBlocks(baked) = %d, want 64 (65 minus the MTP block)", got)
	}
	if got := mainBlocks(plain); got != 65 {
		t.Errorf("mainBlocks(plain) = %d, want 65", got)
	}
	// Name convention still works for a not-yet-downloaded file.
	if !isMTPFile("/nowhere/Qwen3.6-27B-MTP-Q4_K_M.gguf") {
		t.Error("-MTP- filename convention must still detect without a readable file")
	}
}

func TestMuseGlimmerSampling(t *testing.T) {
	m := FamilySamplingArgs("/models/Muse-Glimmer-30B-UD-Q4_K_XL.gguf")
	if len(m) == 0 || m[1] != "1.0" {
		t.Errorf("muse-glimmer should get temp 1.0 sampling, got %v", m)
	}
}
