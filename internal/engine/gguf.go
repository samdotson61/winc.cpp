package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
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
