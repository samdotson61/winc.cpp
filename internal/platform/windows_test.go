//go:build windows

package platform

import "testing"

// TestRegistryVRAM exercises the registry VRAM read end to end. The value is
// hardware-dependent (0 on a machine with no display adapter), so it only asserts
// the call succeeds and is non-negative; run with -v to see the detected MB.
func TestRegistryVRAM(t *testing.T) {
	mb := detectVRAMMBRegistry()
	t.Logf("registry qwMemorySize VRAM = %d MB", mb)
	if mb < 0 {
		t.Fatalf("negative VRAM: %d", mb)
	}
}
