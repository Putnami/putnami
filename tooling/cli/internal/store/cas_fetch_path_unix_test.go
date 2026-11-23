//go:build unix

package store

import "testing"

// TestMaterializeRestoresAColonPathOnUnix pins that the Windows-only refusal
// of a colon leaves Unix restores unchanged: a colon is an ordinary character
// in a Unix file name.
func TestMaterializeRestoresAColonPathOnUnix(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	hit, f := distHit(t, hashA, map[string]string{"a:b.js": "remote artifact bytes"})
	entry, err := s.MaterializeTaskEntry(hit, f.fetch)
	if err != nil {
		t.Fatalf("MaterializeTaskEntry: %v", err)
	}
	if got := readDist(t, entry, "a:b.js"); got != "remote artifact bytes" {
		t.Errorf("restored file = %q", got)
	}
}
