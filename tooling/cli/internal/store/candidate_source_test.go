package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A `git:**` listing taken from a run's cut carries each path's index mode
// into the digest: where the host stores no executable bit, the digest of an
// executable index entry differs from a regular one, whatever the file
// system says.
func TestTheCutCarriesTheIndexModeIntoTheDigest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digestOf := func(mode string) []byte {
		t.Helper()
		cut := func(string) (CandidateCut, bool) {
			return CandidateCut{Paths: []string{"run.sh"}, Modes: map[string]string{"run.sh": mode}, Unmerged: map[string]bool{}}, true
		}
		files, _, _, err := collectGitFilesWith(cut, dir, []string{"git:**"}, nil)
		if err != nil || len(files) != 1 || files[0].indexMode != mode {
			t.Fatalf("listing = %+v, %v: want run.sh with mode %s", files, err, mode)
		}
		return gitCandidateDigest(files[0], make([]byte, 32<<10), false)
	}
	if bytes.Equal(digestOf("100755"), digestOf("100644")) {
		t.Fatal("the index mode of the cut did not reach the digest")
	}
}
