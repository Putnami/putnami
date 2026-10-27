package parse

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const coverageMergeRequirement = "coverage-profile-lists-each-block-once"

// TestMergeCoverageBlocksMergesRepeatsLikeGoToolCover pins the merge rule of
// `go tool cover`: a repeated block keeps its first place, its counts are
// ORed in set mode and added in count and atomic mode, and a block listed
// once keeps its line byte for byte.
func TestMergeCoverageBlocksMergesRepeatsLikeGoToolCover(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", coverageMergeRequirement, "repeated-blocks-merge-as-go-tool-cover-merges-them")
	lines := []string{
		"example.com/a/a.go:3.24,3.34 1 0",
		"example.com/a/a.go:5.27,5.37 2 1",
		"example.com/b/b.go:7.1,9.2 4 0",
		"example.com/a/a.go:3.24,3.34 1 1",
		"example.com/a/a.go:5.27,5.37 2 1",
		"example.com/a/a.go:3.24,3.34 1 0",
	}
	want := []string{
		"example.com/a/a.go:3.24,3.34 1 1",
		"example.com/a/a.go:5.27,5.37 2 1",
		"example.com/b/b.go:7.1,9.2 4 0",
	}
	if got := MergeCoverageBlocks("set", lines); !slices.Equal(got, want) {
		t.Errorf("MergeCoverageBlocks(set) =\n%q\nwant the counts ORed\n%q", got, want)
	}

	counted := []string{
		"example.com/a/a.go:3.24,3.34 1 0",
		"example.com/a/a.go:5.27,5.37 2 3",
		"example.com/b/b.go:7.1,9.2 4 0",
		"example.com/a/a.go:3.24,3.34 1 1",
		"example.com/a/a.go:5.27,5.37 2 4",
		"example.com/a/a.go:3.24,3.34 1 0",
	}
	want = []string{
		"example.com/a/a.go:3.24,3.34 1 1",
		"example.com/a/a.go:5.27,5.37 2 7",
		"example.com/b/b.go:7.1,9.2 4 0",
	}
	for _, mode := range []string{"count", "atomic"} {
		if got := MergeCoverageBlocks(mode, counted); !slices.Equal(got, want) {
			t.Errorf("MergeCoverageBlocks(%s) =\n%q\nwant the counts added\n%q", mode, got, want)
		}
	}
}

// TestMergeCoverageBlocksKeepsWhatItDoesNotMerge pins that the merge only
// removes repeats: a profile without one comes back unchanged, a line that is
// not a block stays in its place, two lines that name one location with
// different statement counts both stay, and no input gives no output.
func TestMergeCoverageBlocksKeepsWhatItDoesNotMerge(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", coverageMergeRequirement, "repeated-blocks-merge-as-go-tool-cover-merges-them")
	unique := []string{
		"example.com/a/a.go:3.24,3.34 1 1",
		"not a block line",
		"",
		"example.com/a/with space.go:1.1,2.1 2 0",
		"example.com/a/a.go:3.24,3.34 2 0",
	}
	if got := MergeCoverageBlocks("set", unique); !slices.Equal(got, unique) {
		t.Fatalf("MergeCoverageBlocks without repeats =\n%q\nwant it unchanged", got)
	}
	if got := MergeCoverageBlocks("set", nil); len(got) != 0 {
		t.Fatalf("MergeCoverageBlocks(nil) = %q, want nothing", got)
	}
}

// TestMergeCoverageProfileRewritesARepeatedProfile pins the file form: the
// first mode line, then each block once, and a coverage parse that reads the
// same numbers before and after.
func TestMergeCoverageProfileRewritesARepeatedProfile(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", coverageMergeRequirement, "repeated-blocks-merge-as-go-tool-cover-merges-them")
	coverFile := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: set\n" +
		"pkg/main.go:10.1,20.1 5 0\n" +
		"pkg/main.go:22.1,30.1 3 0\n" +
		"\n" +
		"mode: set\n" +
		"pkg/main.go:10.1,20.1 5 1\n" +
		"pkg/main.go:22.1,30.1 3 0\n"
	if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatal(err)
	}

	if err := MergeCoverageProfile(coverFile); err != nil {
		t.Fatalf("MergeCoverageProfile: %v", err)
	}

	got, err := os.ReadFile(coverFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "mode: set\npkg/main.go:10.1,20.1 5 1\npkg/main.go:22.1,30.1 3 0\n"
	if string(got) != want {
		t.Fatalf("merged profile =\n%s\nwant\n%s", got, want)
	}
	after, err := CoverageDetails(coverFile)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalStatements != before.TotalStatements || after.CoveredStatements != before.CoveredStatements || after.Percentage != before.Percentage {
		t.Fatalf("coverage after the merge = %d/%d (%v%%), want %d/%d (%v%%)",
			after.CoveredStatements, after.TotalStatements, after.Percentage,
			before.CoveredStatements, before.TotalStatements, before.Percentage)
	}
	entries, err := os.ReadDir(filepath.Dir(coverFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries after the merge, want the profile alone", len(entries))
	}
}

// TestMergeCoverageProfileLeavesAProfileWithoutRepeats pins that a profile
// without a repeated block, or without a mode line, keeps its bytes, and that
// a missing profile is reported, not created.
func TestMergeCoverageProfileLeavesAProfileWithoutRepeats(t *testing.T) {
	dir := t.TempDir()
	for name, profile := range map[string]string{
		"unique.out":  "mode: count\npkg/main.go:10.1,20.1 5 2\n\npkg/main.go:22.1,30.1 3 0\n",
		"no-mode.out": "pkg/main.go:10.1,20.1 5 2\npkg/main.go:10.1,20.1 5 2\n",
		"empty.out":   "",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := MergeCoverageProfile(path); err != nil {
			t.Fatalf("MergeCoverageProfile(%s): %v", name, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != profile {
			t.Errorf("%s = %q after the merge, want it unchanged %q", name, got, profile)
		}
	}

	missing := filepath.Join(dir, "missing.out")
	if err := MergeCoverageProfile(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("MergeCoverageProfile(missing) = %v, want a not-exist error", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("MergeCoverageProfile created the missing profile: %v", err)
	}
}

func TestCoverageMode(t *testing.T) {
	for line, want := range map[string]string{
		"mode: set":     "set",
		"mode: atomic ": "atomic",
		"mode:count":    "count",
	} {
		if got := CoverageMode(line); got != want {
			t.Errorf("CoverageMode(%q) = %q, want %q", line, got, want)
		}
	}
}
