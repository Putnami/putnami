//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaterializeRestoresAColonPathOnUnix pins that the Windows-only refusal
// of a colon leaves Unix restores unchanged: a colon is an ordinary character
// in a Unix file name.
func TestMaterializeRestoresAColonPathOnUnix(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	if _, err := materializeOne(t, s, "dist/a:b.js"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	got, err := s.Get(hashA)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (entry=%v)", err, got)
	}
	if data, err := os.ReadFile(filepath.Join(got.FilesDir, "dist", "a:b.js")); err != nil || string(data) != "remote artifact bytes" {
		t.Errorf("restored file = %q, %v", data, err)
	}
}
