package store

import (
	"path/filepath"
	"testing"
)

// A write the run's exclusion places where Git lists no candidate keeps every
// memoized digest that reads only Git candidates where it lies, so a key after
// it lists no candidate again. A digest that also reads other files the write
// reaches is dropped, and so is every digest the write reaches when the
// exclusion answers no (tooling/cli ADR 0106).
func TestAWriteWhereGitListsNoCandidateKeepsTheCandidateDigests(t *testing.T) {
	for _, excluded := range []bool{true, false} {
		dir := t.TempDir()
		notes := filepath.Join(dir, "notes.txt")
		writeRegionFile(t, notes, "v1")
		writeRegionFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")
		cm := NewCacheManager(nil)
		cm.SetCandidateSource(func(asked string) (CandidateCut, bool) {
			return CandidateCut{Paths: []string{"notes.txt"}, Modes: map[string]string{"notes.txt": "100644"}, Unmerged: map[string]bool{}}, asked == dir
		})
		written := filepath.Join(dir, ".putnami", "out", "app", "report.txt")
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
		candidates, apart, mixed := digest("git:**"), digest("git:**", "src/**"), digest("git:**", "**/*.json")

		// The memo answers a digest from before this edit until an
		// invalidation drops it.
		writeRegionFile(t, notes, "v2")
		if digest("git:**") != candidates {
			t.Fatal("the memo did not hold the digest before the edit, so this test cannot prove anything")
		}
		cm.InvalidateFileHashesUnder([]string{written})

		if len(asked) != 1 || len(asked[0]) != 1 || asked[0][0] != written {
			t.Fatalf("the exclusion was asked %v, want once for %s", asked, written)
		}
		for _, check := range []struct {
			name  string
			got   string
			was   string
			keeps bool
		}{
			{"a digest of Git candidates only", digest("git:**"), candidates, excluded},
			{"a digest whose other files lie elsewhere", digest("git:**", "src/**"), apart, excluded},
			{"a digest whose other files the write reaches", digest("git:**", "**/*.json"), mixed, false},
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
