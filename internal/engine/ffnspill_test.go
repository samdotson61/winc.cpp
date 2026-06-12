package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winc/internal/config"
	"winc/internal/platform"
)

// seedFFN registers fake GGUF facts for a path so the FFN plan/args logic is
// testable without multi-GB fixtures (NTFS allocates Truncate-extended files
// for real -- fixtures must stay tiny).
func seedFFN(t *testing.T, name string, sizeMB int, blocks int, ffnTotalMB int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	blockCountCache.Store(p, blocks)
	ffnBytesCache.Store(p, ffnTotalMB<<20)
	return p
}

func TestFFNSpillArgs(t *testing.T) {
	dense := seedFFN(t, "Dense-9B-Q4_K_M.gguf", 50, 32, 256)
	args := FFNSpillArgs(dense, 4)
	want := `blk\.(28|29|30|31)\.ffn_.*=CPU`
	if len(args) != 2 || args[0] != "-ot" || args[1] != want {
		t.Fatalf("FFNSpillArgs = %v, want [-ot %s]", args, want)
	}
	// An MTP head's extra block is never targeted: its tensors only load for
	// speculative decoding, which the spill stage runs without.
	mtp := seedFFN(t, "Big-27B-MTP-Q5_K_M.gguf", 50, 65, 11767)
	args = FFNSpillArgs(mtp, 4)
	if want := `blk\.(60|61|62|63)\.ffn_.*=CPU`; len(args) != 2 || args[1] != want {
		t.Fatalf("MTP head block must be excluded: %v, want %s", args, want)
	}
	// Requests past the block count clamp to every main block.
	args = FFNSpillArgs(dense, 99)
	if len(args) != 2 || !strings.HasPrefix(args[1], `blk\.(0|1|`) || !strings.Contains(args[1], "|31)") {
		t.Fatalf("over-large k should clamp to all main blocks: %v", args)
	}
	if FFNSpillArgs(dense, 0) != nil {
		t.Fatal("k=0 must produce no args")
	}
	if FFNSpillArgs(filepath.Join(t.TempDir(), "missing.gguf"), 4) != nil {
		t.Fatal("unknown block count must produce no args")
	}
}

func TestFFNSpillPlan(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.Mtp = "off"        // the spill stage always plans with the draft off
	cfg.Performance.CacheType = "q8_0" // pin the factor (auto would downshift on this starved fixture)
	// A 4 GB-class scenario scaled to fixture-safe sizes: 600 MB card, 50 MB
	// model, 32 blocks averaging 8 MB of FFN weights each.
	hw := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 600, GPUs: []platform.GPUDevice{{TotalMB: 600}}}
	m := seedFFN(t, "Small-Dense-Q4_K_M.gguf", 50, 32, 256)

	// have = 600 - 50 - (512+50/8) = 32 MB; 16384 tokens of q8 KV needs 256 MB
	// -> deficit 224 -> ceil(224/8)+1 = 29 blocks: plannable (<= 32).
	k, blocks := FFNSpillPlan(&cfg, hw, m, 16384)
	if blocks != 32 || k != 29 {
		t.Fatalf("deficit plan = (%d, %d), want (29, 32)", k, blocks)
	}
	// 98304 tokens needs 1536 MB of KV -- more than every FFN block covers.
	if k, blocks = FFNSpillPlan(&cfg, hw, m, 98304); k <= blocks {
		t.Fatalf("impossible window must report k(%d) > blocks(%d)", k, blocks)
	}
	// Ample VRAM -> no spill needed.
	ample := platform.Hardware{GPUVendor: "nvidia", VRAMMB: 16000, GPUs: []platform.GPUDevice{{TotalMB: 16000}}}
	if k, _ = FFNSpillPlan(&cfg, ample, m, 16384); k != 0 {
		t.Fatalf("fitting budget must plan no spill, got %d", k)
	}
	// MoE models keep their own offload path (--cpu-moe).
	moe := seedFFN(t, "Qwen3.6-35B-A3B-UD-Q4_K_M.gguf", 50, 48, 200)
	if k, _ = FFNSpillPlan(&cfg, hw, moe, 16384); k != 0 {
		t.Fatalf("MoE must never FFN-spill, got %d", k)
	}
	// Explicit gpu_layers / unified memory / CPU-only never plan.
	exp := cfg
	exp.Performance.GpuLayers = "20"
	if k, _ = FFNSpillPlan(&exp, hw, m, 16384); k != 0 {
		t.Fatalf("explicit gpu_layers must never FFN-spill, got %d", k)
	}
	mac := platform.Hardware{GPUVendor: "apple", Unified: true, VRAMMB: 24000, GPUs: []platform.GPUDevice{{TotalMB: 24000}}}
	if k, _ = FFNSpillPlan(&cfg, mac, m, 16384); k != 0 {
		t.Fatalf("unified memory must never FFN-spill, got %d", k)
	}
}

func TestServerArgsFFNSpill(t *testing.T) {
	cfg := config.Defaults()
	cfg.Performance.FFNSpill = 4
	hw := platform.Hardware{OS: "windows", GPUVendor: "nvidia", VRAMMB: 4096, GPUs: []platform.GPUDevice{{TotalMB: 4096}}}
	m := seedFFN(t, "Dense-4B-Q4_K_M.gguf", 50, 32, 256)
	s := strings.Join(ServerArgs(&cfg, hw, m, 8080, "", 49152), " ")
	if !strings.Contains(s, "-ngl 99") {
		t.Errorf("FFN spill must pin -ngl 99: %s", s)
	}
	if !strings.Contains(s, `-ot blk\.(28|29|30|31)\.ffn_.*=CPU`) {
		t.Errorf("FFN spill tensor override missing: %s", s)
	}
	if strings.Contains(s, "--tensor-split") {
		t.Errorf("FFN spill must not pass a tensor split: %s", s)
	}
	// The spill placement is a pinned load -> the placement gate covers it.
	if !ForcedFullGPU(&cfg, hw, m) {
		t.Error("FFN spill loads must be gate-covered (ForcedFullGPU)")
	}
	// The spilled bytes the gate excuses from residency.
	if got := FFNSpillMB(m, 4); got != 32 {
		t.Errorf("FFNSpillMB = %d, want 32", got)
	}
	if got := FFNSpillMB(m, 99); got != 256 {
		t.Errorf("clamped FFNSpillMB = %d, want 256", got)
	}
}
