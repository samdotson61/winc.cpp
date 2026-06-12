package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// systemFirstRaise matches the strict "system message must be at the beginning" guard
// some 2026 templates (Qwen3.5) added. It breaks llama.cpp's tool-call parser
// generation: the probe render passes messages that trip the guard, so the server
// 400s on EVERY request (Claude Code always sends tools). Other raise_exception guards
// in the template are intentionally left intact.
var systemFirstRaise = regexp.MustCompile(`(?s)\{\{-?\s*raise_exception\([^)]*?beginning[^)]*?\)\s*-?\}\}`)

var chatTemplateCache sync.Map // modelPath -> []string

// ChatTemplateArgs returns "--chat-template-file <patched>" when a model's embedded
// chat template contains the parser-breaking system-position guard, having written the
// patched template next to the model. Returns nil when the template is fine or absent,
// so models whose templates already work (e.g. Qwen3.6) are launched unchanged.
func ChatTemplateArgs(modelPath string) []string {
	if v, ok := chatTemplateCache.Load(modelPath); ok {
		return v.([]string)
	}
	args := computeChatTemplateArgs(modelPath)
	chatTemplateCache.Store(modelPath, args)
	return args
}

func computeChatTemplateArgs(modelPath string) []string {
	tmpl, err := ChatTemplate(modelPath)
	if err != nil || tmpl == "" || !systemFirstRaise.MatchString(tmpl) {
		return nil
	}
	out := modelPath + ".winc.jinja"
	if err := os.WriteFile(out, []byte(systemFirstRaise.ReplaceAllString(tmpl, "")), 0o644); err != nil {
		return nil
	}
	return []string{"--chat-template-file", out}
}

const ggufMagic = 0x46554747 // "GGUF" little-endian

var blockCountCache sync.Map // modelPath -> int

// BlockCount returns the model's transformer block count from GGUF metadata
// ("<arch>.block_count"), or 0 if unavailable. llama.cpp's -ngl counts these
// blocks plus one output layer, so "every layer on the GPU" means block_count+1.
func BlockCount(path string) int {
	if v, ok := blockCountCache.Load(path); ok {
		return v.(int)
	}
	n, err := readBlockCount(path)
	if err != nil {
		n = 0
	}
	blockCountCache.Store(path, n)
	return n
}

func readBlockCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)

	var magic, version uint32
	var nTensors, nKV uint64
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return 0, err
	}
	if magic != ggufMagic {
		return 0, fmt.Errorf("not a GGUF file")
	}
	if err := readLE(r, &version, &nTensors, &nKV); err != nil {
		return 0, err
	}
	for i := uint64(0); i < nKV; i++ {
		key, err := ggufString(r)
		if err != nil {
			return 0, err
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return 0, err
		}
		if strings.HasSuffix(key, ".block_count") {
			return ggufUint(r, vtype)
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

var ffnBytesCache sync.Map // modelPath -> int64 (total bytes of all blk.*.ffn_* tensors)

// FFNTotalBytes returns the EXACT on-disk bytes of every feed-forward weight
// tensor (blk.N.ffn_*) in a GGUF, or 0 if unavailable. Sizes come from the
// tensor table's offset deltas -- no per-quant type table to maintain, so any
// current or future quantization reads correctly. This is the byte pool the
// dense FFN-spill placement can move to RAM: for the qwen35 family it is
// ~49-61% of the file, far more relief per layer than whole-layer offload and
// it leaves every attention/SSM tensor (and so the whole KV cache) on the GPU.
func FFNTotalBytes(path string) int64 {
	if v, ok := ffnBytesCache.Load(path); ok {
		return v.(int64)
	}
	n, err := readFFNTotalBytes(path)
	if err != nil {
		n = 0
	}
	ffnBytesCache.Store(path, n)
	return n
}

// FFNLayerMB is the average per-block FFN weight size in MB (0 if unknown).
func FFNLayerMB(path string) int {
	blocks := BlockCount(path)
	total := FFNTotalBytes(path)
	if blocks <= 0 || total <= 0 {
		return 0
	}
	return int(total / int64(blocks) >> 20)
}

func readFFNTotalBytes(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	cr := &countingReader{r: f}
	r := bufio.NewReaderSize(cr, 1<<20)

	var magic, version uint32
	var nTensors, nKV uint64
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return 0, err
	}
	if magic != ggufMagic {
		return 0, fmt.Errorf("not a GGUF file")
	}
	if err := readLE(r, &version, &nTensors, &nKV); err != nil {
		return 0, err
	}
	alignment := int64(32) // GGUF default; overridden by general.alignment
	for i := uint64(0); i < nKV; i++ {
		key, err := ggufString(r)
		if err != nil {
			return 0, err
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return 0, err
		}
		if key == "general.alignment" {
			a, err := ggufUint(r, vtype)
			if err != nil {
				return 0, err
			}
			if a > 0 {
				alignment = int64(a)
			}
			continue
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return 0, err
		}
	}
	if nTensors == 0 || nTensors > 1<<20 {
		return 0, fmt.Errorf("absurd gguf tensor count %d", nTensors)
	}
	type tinfo struct {
		offset int64
		ffn    bool
	}
	infos := make([]tinfo, 0, nTensors)
	for i := uint64(0); i < nTensors; i++ {
		name, err := ggufString(r)
		if err != nil {
			return 0, err
		}
		var nDims uint32
		if err := binary.Read(r, binary.LittleEndian, &nDims); err != nil {
			return 0, err
		}
		if nDims > 8 {
			return 0, fmt.Errorf("absurd tensor rank %d", nDims)
		}
		// dims (u64 each) + type (u32) are not needed for offset-delta sizing
		if _, err := io.CopyN(io.Discard, r, int64(nDims)*8+4); err != nil {
			return 0, err
		}
		var offset uint64
		if err := binary.Read(r, binary.LittleEndian, &offset); err != nil {
			return 0, err
		}
		infos = append(infos, tinfo{
			offset: int64(offset),
			ffn:    strings.HasPrefix(name, "blk.") && strings.Contains(name, ".ffn_"),
		})
	}
	// Everything read so far is the header; tensor data starts at the next
	// alignment boundary. Account for bytes buffered ahead by the bufio layers.
	headerEnd := cr.n - int64(r.Buffered())
	dataStart := (headerEnd + alignment - 1) / alignment * alignment
	dataSize := st.Size() - dataStart
	if dataSize <= 0 {
		return 0, fmt.Errorf("gguf data section missing")
	}
	// Tensor size = gap to the next tensor's offset (offsets are relative to the
	// data section). Sort by offset; the last tensor runs to the end of the file.
	sort.Slice(infos, func(a, b int) bool { return infos[a].offset < infos[b].offset })
	var total int64
	for i, t := range infos {
		if !t.ffn {
			continue
		}
		end := dataSize
		if i+1 < len(infos) {
			end = infos[i+1].offset
		}
		if end > t.offset {
			total += end - t.offset
		}
	}
	return total, nil
}

// countingReader counts bytes consumed from the underlying reader.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ggufUint reads an integer-typed GGUF scalar as an int.
func ggufUint(r *bufio.Reader, vtype uint32) (int, error) {
	switch vtype {
	case 4: // uint32
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return int(v), err
	case 5: // int32
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return int(v), err
	case 10: // uint64
		var v uint64
		err := binary.Read(r, binary.LittleEndian, &v)
		return int(v), err
	case 11: // int64
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return int(v), err
	}
	return 0, fmt.Errorf("block_count has non-integer type %d", vtype)
}

// ChatTemplate extracts the embedded "tokenizer.chat_template" string from a GGUF
// file, or "" (no error) if the model has none. It reads only the metadata header.
func ChatTemplate(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)

	var magic, version uint32
	var nTensors, nKV uint64
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return "", err
	}
	if magic != ggufMagic {
		return "", fmt.Errorf("not a GGUF file")
	}
	if err := readLE(r, &version, &nTensors, &nKV); err != nil {
		return "", err
	}
	for i := uint64(0); i < nKV; i++ {
		key, err := ggufString(r)
		if err != nil {
			return "", err
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return "", err
		}
		if key == "tokenizer.chat_template" {
			if vtype != 8 {
				return "", fmt.Errorf("chat_template is not a string (type %d)", vtype)
			}
			return ggufString(r)
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return "", err
		}
	}
	return "", nil
}

func readLE(r io.Reader, vals ...any) error {
	for _, v := range vals {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func ggufString(r *bufio.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 64<<20 {
		return "", fmt.Errorf("absurd gguf string length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

var ggufScalarSize = map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

func ggufSkipValue(r *bufio.Reader, vtype uint32) error {
	if sz, ok := ggufScalarSize[vtype]; ok {
		_, err := io.CopyN(io.Discard, r, sz)
		return err
	}
	switch vtype {
	case 8: // string
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(n))
		return err
	case 9: // array
		var subtype uint32
		var count uint64
		if err := readLE(r, &subtype, &count); err != nil {
			return err
		}
		if sz, ok := ggufScalarSize[subtype]; ok {
			_, err := io.CopyN(io.Discard, r, int64(count)*sz)
			return err
		}
		if subtype == 8 { // array of strings
			for j := uint64(0); j < count; j++ {
				if err := ggufSkipValue(r, 8); err != nil {
					return err
				}
			}
			return nil
		}
		return errors.New("unsupported gguf array subtype")
	}
	return fmt.Errorf("unsupported gguf value type %d", vtype)
}
