package tscscript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPrefersTheLibraryConfig(t *testing.T) {
	dir := t.TempDir()
	if got := DefaultConfig(dir); got != "tsconfig.json" {
		t.Errorf("without tsconfig.lib.json = %q, want tsconfig.json", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.lib.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfig(dir); got != "tsconfig.lib.json" {
		t.Errorf("with tsconfig.lib.json = %q", got)
	}
}
