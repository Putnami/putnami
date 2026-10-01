//go:build windows

package store

import (
	"strings"
	"testing"
)

// TestMaterializeRefusesAPathWindowsCannotName pins R5-F1: a manifest path the
// cache protocol admits but Windows cannot name as a plain file (a colon names
// an alternate data stream or a drive, NUL and its kin name devices) fails the
// restore by name before any blob is fetched.
func TestMaterializeRefusesAPathWindowsCannotName(t *testing.T) {
	for _, rel := range []string{"a:b", "dist/x.js:stream", "C:/x", "NUL", "dist/nul.txt"} {
		t.Run(rel, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			f, err := materializeOne(t, s, rel)
			if err == nil || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), "is not a local path on this host") {
				t.Fatalf("Materialize(%q) = %v, want the path refused by name", rel, err)
			}
			if fetched := f.distinctOpened(); len(fetched) != 0 {
				t.Errorf("fetched %v before refusing the manifest", fetched)
			}
		})
	}
}
