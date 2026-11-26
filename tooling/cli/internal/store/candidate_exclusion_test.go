package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A write the run's exclusion places where Git lists no candidate keeps every
// memoized digest that reads only Git candidates where it lies, so a key after
// it lists no candidate again. A digest whose other patterns select the written
// file is dropped, and so is every digest that can select it when the
// exclusion answers no (tooling/cli ADR 0106, ADR 0109).
func TestAWriteWhereGitListsNoCandidateKeepsTheCandidateDigests(t *testing.T) {
	for _, excluded := range []bool{true, false} {
		dir := t.TempDir()
		notes := filepath.Join(dir, "notes.txt")
		writeRegionFile(t, notes, "v1")
		writeRegionFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")
		cm := NewCacheManager(nil)
		written := filepath.Join(dir, ".putnami", "out", "app", "report.txt")
		// The cut lists the report once it is written where Git does not
		// exclude it, as `git ls-files --others` would.
		cm.SetCandidateSource(func(asked string) (CandidateCut, bool) {
			read := CandidateCut{Paths: []string{"notes.txt"}, Modes: map[string]string{"notes.txt": "100644"}, Unmerged: map[string]bool{}}
			if _, err := os.Stat(written); err == nil && !excluded {
				read.Paths = []string{".putnami/out/app/report.txt", "notes.txt"}
			}
			return read, asked == dir
		})
		var asked [][]string
		cm.SetCandidateExclusion(func(paths []string) bool {
			asked = append(asked, paths)
			return excluded
		})
		digest := func(patterns ...string) string {
			t.Helper()
			hash, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			return hash
		}
		candidates, apart, skipped, mixed := digest("git:**"), digest("git:**", "src/**"),
			digest("git:**", "**/*.txt"), digest("git:**", ".putnami/out/app/*.txt")

		// The memo answers a digest from before this edit until an
		// invalidation drops it.
		writeRegionFile(t, notes, "v2")
		if digest("git:**") != candidates {
			t.Fatal("the memo did not hold the digest before the edit, so this test cannot prove anything")
		}
		writeRegionFile(t, written, "report")
		cm.InvalidateFileHashesUnder([]string{written})

		// Every digest here listed its candidates from the cut, so the cut
		// read after the write decides, and the exclusion is not asked.
		if len(asked) != 0 {
			t.Fatalf("the exclusion was asked %v, want never", asked)
		}
		for _, check := range []struct {
			name  string
			got   string
			was   string
			keeps bool
		}{
			{"a digest of Git candidates only", digest("git:**"), candidates, excluded},
			{"a digest whose other files lie elsewhere", digest("git:**", "src/**"), apart, excluded},
			{"a digest whose recursive walk skips .putnami", digest("git:**", "**/*.txt"), skipped, excluded},
			{"a digest whose other patterns select the written file", digest("git:**", ".putnami/out/app/*.txt"), mixed, false},
		} {
			if kept := check.got == check.was; kept != check.keeps {
				t.Errorf("excluded %v: %s kept its entry = %v, want %v", excluded, check.name, kept, check.keeps)
			}
		}
	}
}

// A write that reaches no digest of Git candidates never asks the exclusion,
// so a run whose keys read no candidate starts no Git for it.
func TestAWriteThatReachesNoCandidateDigestAsksNothing(t *testing.T) {
	dir := t.TempDir()
	writeRegionFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")
	cm := NewCacheManager(nil)
	cm.SetCandidateExclusion(func([]string) bool {
		t.Error("the exclusion was asked for a write that reaches no digest of Git candidates")
		return true
	})
	if _, err := cm.HashFiles(dir, []string{"src/**"}, nil, ProjectConfigScope{}); err != nil {
		t.Fatal(err)
	}
	cm.InvalidateFileHashesUnder([]string{filepath.Join(dir, ".putnami", "out", "report.txt")})
	cm.InvalidateFileHashesUnder([]string{filepath.Join(dir, "src", "main.ts")})
}
