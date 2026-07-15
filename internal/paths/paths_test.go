package paths

import (
	"path/filepath"
	"testing"
)

func TestJournalDirUnderInstallDir(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir())
	if got, want := JournalDir(), filepath.Join(InstallDir(), "journal"); got != want {
		t.Fatalf("JournalDir() = %q, want %q", got, want)
	}
}
