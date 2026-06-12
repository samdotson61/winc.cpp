package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeSyntheticGGUF builds a minimal valid GGUF v3 file with a real tensor
// table so the offset-delta FFN sizing is tested end to end: four tensors,
// the two ffn_* ones spanning 96 bytes each.
func writeSyntheticGGUF(t *testing.T) string {
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
	w(uint64(4)) // n_tensors
	w(uint64(2)) // n_kv
	// KV 1: general.alignment = 32 (u32, type id 4)
	str("general.alignment")
	w(uint32(4))
	w(uint32(32))
	// KV 2: test.block_count = 1 (u32) -- exercises the BlockCount scan too
	str("test.block_count")
	w(uint32(4))
	w(uint32(1))
	// Tensor table: name, n_dims, dims, type, offset (offsets 32-aligned).
	tensor := func(name string, offset uint64) {
		str(name)
		w(uint32(1)) // n_dims
		w(uint64(7)) // dims[0] -- ignored by the offset-delta sizing
		w(uint32(0)) // type f32 -- ignored too
		w(offset)
	}
	tensor("blk.0.attn_qkv.weight", 0)  // 64 bytes (next offset)
	tensor("blk.0.ffn_gate.weight", 64) // 96 bytes
	tensor("blk.0.ffn_up.weight", 160)  // 96 bytes
	tensor("output.weight", 256)        // 64 bytes (to end of data)
	// Pad the header to the 32-byte alignment boundary, then the data section.
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, 320)) // data: 256 + 64
	p := filepath.Join(t.TempDir(), "synthetic.gguf")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFFNTotalBytesSynthetic(t *testing.T) {
	p := writeSyntheticGGUF(t)
	if got := FFNTotalBytes(p); got != 192 {
		t.Fatalf("FFNTotalBytes = %d, want 192 (96+96 via offset deltas)", got)
	}
	if got := BlockCount(p); got != 1 {
		t.Fatalf("BlockCount = %d, want 1", got)
	}
	// Not a GGUF -> 0, never an error surfaced to sizing.
	bad := filepath.Join(t.TempDir(), "not.gguf")
	if err := os.WriteFile(bad, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FFNTotalBytes(bad); got != 0 {
		t.Fatalf("non-GGUF must read 0 FFN bytes, got %d", got)
	}
}
