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
	"strconv"
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

var expertCountCache sync.Map // modelPath -> int (-1 = metadata unreadable)

// ExpertCount returns the model's MoE expert count from GGUF metadata
// ("<arch>.expert_count"): >0 for a Mixture-of-Experts model, 0 when the model is
// dense (the key is absent), and -1 when the metadata could not be read at all --
// the file is missing, truncated, or only a bare filename was passed. Callers must
// treat 0 and -1 differently: 0 is an authoritative "dense", -1 means "unknown".
func ExpertCount(path string) int {
	if v, ok := expertCountCache.Load(path); ok {
		return v.(int)
	}
	n, err := readExpertCount(path)
	if err != nil {
		n = -1
	}
	expertCountCache.Store(path, n)
	return n
}

func readExpertCount(path string) (int, error) {
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
		// "<arch>.expert_count" only -- NOT expert_used_count / expert_shared_count.
		if strings.HasSuffix(key, ".expert_count") {
			return ggufUint(r, vtype)
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return 0, err
		}
	}
	return 0, nil // read cleanly, no expert_count key -> dense
}

var mtpLayersCache sync.Map // modelPath -> int (0 = none / unreadable)

// MTPLayers returns the count of Multi-Token-Prediction layers baked into a
// GGUF ("<arch>.nextn_predict_layers"), or 0 when the model has none or the
// metadata can't be read. Qwen3.8 ships its MTP head inside every standard
// quant (no separate "-MTP" repo), so the filename says nothing -- the
// metadata does. Read once per path; the header walk is cheap and cached.
func MTPLayers(path string) int {
	if v, ok := mtpLayersCache.Load(path); ok {
		return v.(int)
	}
	n, err := readMTPLayers(path)
	if err != nil || n < 0 {
		n = 0
	}
	mtpLayersCache.Store(path, n)
	return n
}

func readMTPLayers(path string) (int, error) {
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
		if strings.HasSuffix(key, ".nextn_predict_layers") {
			return ggufUint(r, vtype)
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

var trainedCtxCache sync.Map // modelPath -> int (0 = unknown)

// TrainedContext returns the context length the model was trained for, from GGUF
// metadata ("<arch>.context_length"), or 0 when it cannot be read. Asking for more
// than this doesn't get it: llama-server caps the slot to n_ctx_train and logs
// "the slot context (N) exceeds the training context of the model (M) - capping",
// so any window winc sizes, reports, or estimates decode for above this describes
// a configuration that never actually runs.
func TrainedContext(path string) int {
	if v, ok := trainedCtxCache.Load(path); ok {
		return v.(int)
	}
	n, err := readTrainedContext(path)
	if err != nil || n < 0 {
		n = 0
	}
	trainedCtxCache.Store(path, n)
	return n
}

func readTrainedContext(path string) (int, error) {
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
		// "<arch>.context_length" exactly -- NOT rope.scaling.original_context_length.
		if strings.HasSuffix(key, ".context_length") {
			return ggufUint(r, vtype)
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

var tokenizerFPCache sync.Map // modelPath -> string ("" = unreadable)

// TokenizerFingerprint returns a short identity for a GGUF's tokenizer --
// "<tokenizer-model>:<vocab-size>:<eos-id>" -- or "" when it cannot be read.
// Two models sharing a fingerprint share a vocabulary, which is the actual
// requirement for one to draft the other in speculative decoding. Matching on
// this instead of on a family name in the filename means any model can be
// drafted by any compatible draft, however it happens to be named.
func TokenizerFingerprint(path string) string {
	if v, ok := tokenizerFPCache.Load(path); ok {
		return v.(string)
	}
	fp, err := readTokenizerFingerprint(path)
	if err != nil {
		fp = ""
	}
	tokenizerFPCache.Store(path, fp)
	return fp
}

func readTokenizerFingerprint(path string) (string, error) {
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
	var model string
	var vocab, eos int
	for i := uint64(0); i < nKV; i++ {
		key, err := ggufString(r)
		if err != nil {
			return "", err
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return "", err
		}
		switch {
		case key == "tokenizer.ggml.model" && vtype == 8:
			if model, err = ggufString(r); err != nil {
				return "", err
			}
		case key == "tokenizer.ggml.eos_token_id":
			if eos, err = ggufUint(r, vtype); err != nil {
				return "", err
			}
		case key == "tokenizer.ggml.tokens" && vtype == 9:
			// Array header carries the vocabulary size; skip the tokens themselves.
			var subtype uint32
			var count uint64
			if err := readLE(r, &subtype, &count); err != nil {
				return "", err
			}
			vocab = int(count)
			if subtype != 8 {
				return "", fmt.Errorf("tokenizer.ggml.tokens is not a string array")
			}
			for j := uint64(0); j < count; j++ {
				if err := ggufSkipValue(r, 8); err != nil {
					return "", err
				}
			}
		default:
			if err := ggufSkipValue(r, vtype); err != nil {
				return "", err
			}
		}
	}
	if model == "" || vocab == 0 {
		return "", fmt.Errorf("no tokenizer metadata")
	}
	return fmt.Sprintf("%s:%d:%d", model, vocab, eos), nil
}

// DraftMismatch reports whether a draft model's tokenizer is KNOWN to differ from
// its target's. It returns false when either fingerprint is unreadable, so an
// unverifiable pair is handed to the engine untouched rather than blocked on a
// guess -- this guard only fires on a mismatch it can actually prove.
func DraftMismatch(targetPath, draftPath string) bool {
	t, d := TokenizerFingerprint(targetPath), TokenizerFingerprint(draftPath)
	return t != "" && d != "" && t != d
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

var moeExpertCache sync.Map // modelPath -> moeExpertStats

type moeExpertStats struct {
	TotalBytes int64 // all blk.*.*_exps.* tensor bytes (routed experts only; shared "shexp" excluded)
	Layers     int   // number of blocks carrying expert tensors
}

// MoEExpertStats returns the EXACT on-disk size of a MoE model's routed-expert
// tensors (blk.N.ffn_*_exps.*) and how many layers carry them, or zeros if
// unavailable. Shared-expert tensors ("shexp") are excluded — the engine's
// --cpu-moe/--n-cpu-moe only move the routed experts, and the shared expert
// runs every token so it must stay in the resident base. This is the byte pool
// the MoE packing plan can place back on the GPU.
func MoEExpertStats(path string) (totalMB, layers int) {
	if v, ok := moeExpertCache.Load(path); ok {
		s := v.(moeExpertStats)
		return int(s.TotalBytes >> 20), s.Layers
	}
	var s moeExpertStats
	seen := map[int]bool{}
	err := walkTensorSizes(path, func(name string, sz int64) {
		if !strings.HasPrefix(name, "blk.") || !strings.Contains(name, "_exps.") {
			return
		}
		s.TotalBytes += sz
		if i := strings.IndexByte(name[4:], '.'); i > 0 {
			if n, err := strconv.Atoi(name[4 : 4+i]); err == nil && !seen[n] {
				seen[n] = true
				s.Layers++
			}
		}
	})
	if err != nil {
		s = moeExpertStats{}
	}
	moeExpertCache.Store(path, s)
	return int(s.TotalBytes >> 20), s.Layers
}

func readFFNTotalBytes(path string) (int64, error) {
	var total int64
	err := walkTensorSizes(path, func(name string, sz int64) {
		if strings.HasPrefix(name, "blk.") && strings.Contains(name, ".ffn_") {
			total += sz
		}
	})
	return total, err
}

// walkTensorSizes reads a GGUF's tensor table and calls fn(name, bytes) for
// every tensor, with sizes from offset deltas (no per-quant type table to
// maintain). Extracted from the v1.21 FFN sizing so MoE expert sizing shares
// one battle-tested parser.
func walkTensorSizes(path string, fn func(name string, bytes int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	cr := &countingReader{r: f}
	r := bufio.NewReaderSize(cr, 1<<20)

	var magic, version uint32
	var nTensors, nKV uint64
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return err
	}
	if magic != ggufMagic {
		return fmt.Errorf("not a GGUF file")
	}
	if err := readLE(r, &version, &nTensors, &nKV); err != nil {
		return err
	}
	alignment := int64(32) // GGUF default; overridden by general.alignment
	for i := uint64(0); i < nKV; i++ {
		key, err := ggufString(r)
		if err != nil {
			return err
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return err
		}
		if key == "general.alignment" {
			a, err := ggufUint(r, vtype)
			if err != nil {
				return err
			}
			if a > 0 {
				alignment = int64(a)
			}
			continue
		}
		if err := ggufSkipValue(r, vtype); err != nil {
			return err
		}
	}
	if nTensors == 0 || nTensors > 1<<20 {
		return fmt.Errorf("absurd gguf tensor count %d", nTensors)
	}
	type tinfo struct {
		offset int64
		name   string
	}
	infos := make([]tinfo, 0, nTensors)
	for i := uint64(0); i < nTensors; i++ {
		name, err := ggufString(r)
		if err != nil {
			return err
		}
		var nDims uint32
		if err := binary.Read(r, binary.LittleEndian, &nDims); err != nil {
			return err
		}
		if nDims > 8 {
			return fmt.Errorf("absurd tensor rank %d", nDims)
		}
		// dims (u64 each) + type (u32) are not needed for offset-delta sizing
		if _, err := io.CopyN(io.Discard, r, int64(nDims)*8+4); err != nil {
			return err
		}
		var offset uint64
		if err := binary.Read(r, binary.LittleEndian, &offset); err != nil {
			return err
		}
		infos = append(infos, tinfo{offset: int64(offset), name: name})
	}
	// Everything read so far is the header; tensor data starts at the next
	// alignment boundary. Account for bytes buffered ahead by the bufio layers.
	headerEnd := cr.n - int64(r.Buffered())
	dataStart := (headerEnd + alignment - 1) / alignment * alignment
	dataSize := st.Size() - dataStart
	if dataSize <= 0 {
		return fmt.Errorf("gguf data section missing")
	}
	// Tensor size = gap to the next tensor's offset (offsets are relative to the
	// data section). Sort by offset; the last tensor runs to the end of the file.
	sort.Slice(infos, func(a, b int) bool { return infos[a].offset < infos[b].offset })
	for i, t := range infos {
		end := dataSize
		if i+1 < len(infos) {
			end = infos[i+1].offset
		}
		if end > t.offset {
			fn(t.name, end-t.offset)
		}
	}
	return nil
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
	return 0, fmt.Errorf("gguf scalar has non-integer type %d", vtype)
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
