package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

// A "<Family>-mmproj.gguf" next to a model turns on --mmproj for every quant
// and MTP variant of that family; vision="off" disables; other families and
// models without a projector get nothing.
func TestVisionArgs(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "Qwen3.6-35B-mmproj.gguf")
	for _, n := range []string{"Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf", "Qwen3.6-35B-A3B-UD-Q5_K_M.gguf", "Qwen3.6-27B-Q4_K_M.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(proj, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults() // vision=auto

	for _, model := range []string{"Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf", "Qwen3.6-35B-A3B-UD-Q5_K_M.gguf"} {
		got := strings.Join(VisionArgs(&cfg, filepath.Join(dir, model)), " ")
		if got != "--mmproj "+proj {
			t.Errorf("%s: got %q, want the family projector", model, got)
		}
	}
	if got := VisionArgs(&cfg, filepath.Join(dir, "Qwen3.6-27B-Q4_K_M.gguf")); got != nil {
		t.Errorf("27B must not pair a 35B projector, got %v", got)
	}
	if r := visionReserveMB(&cfg, filepath.Join(dir, "Qwen3.6-35B-A3B-UD-Q5_K_M.gguf")); r < 512 {
		t.Errorf("active vision should reserve VRAM, got %d", r)
	}
	cfg.Performance.Vision = "off"
	if got := VisionArgs(&cfg, filepath.Join(dir, "Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf")); got != nil {
		t.Errorf("vision=off must disable, got %v", got)
	}
	if r := visionReserveMB(&cfg, filepath.Join(dir, "Qwen3.6-35B-A3B-MTP-UD-Q4_K_M.gguf")); r != 0 {
		t.Errorf("vision=off must reserve nothing, got %d", r)
	}
}

// A loaded projector suppresses DRAFT speculation (draft batches break on
// image embeddings) but keeps model-free ngram -- and drops the draft reserves.
func TestVisionSuppressesDraftSpec(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"Qwen3.5-4B-Q4_K_M.gguf", "Qwen3.5-4B-DFlash.gguf", "Qwen3.5-4B-mmproj.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	hw := platformHW()
	model := filepath.Join(dir, "Qwen3.5-4B-Q4_K_M.gguf")
	got := strings.Join(SpecArgs(&cfg, hw, model, "", true), " ")
	if strings.Contains(got, "dflash") || strings.Contains(got, "draft-mtp") {
		t.Errorf("vision-active launch must drop draft speculation, got %q", got)
	}
	if got != "--spec-type ngram-simple" {
		t.Errorf("ngram must survive vision, got %q", got)
	}
	if r := dflashReserveMB(&cfg, model); r != 0 {
		t.Errorf("suppressed dflash must reserve nothing, got %d", r)
	}
	// vision off -> the draft head engages again.
	cfg.Performance.Vision = "off"
	if got := strings.Join(SpecArgs(&cfg, hw, model, "", true), " "); !strings.Contains(got, "draft-dflash") {
		t.Errorf("vision=off should restore dflash, got %q", got)
	}
}

func platformHW() platform.Hardware { return platform.Hardware{} }
