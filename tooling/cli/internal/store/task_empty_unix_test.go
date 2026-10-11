//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The output directory keeps its permission bits: emptying it never replaces
// it with a directory of the engine's own making.
func TestEmptyTaskOutput_KeepsTheDirectoryMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"stale.html": "stale"})
	if err := os.Chmod(dest, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := EmptyTaskOutput(dest, filepath.Join(t.TempDir(), "scratch")); err != nil {
		t.Fatalf("EmptyTaskOutput: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("mode = %o, want 750", got)
	}
	assertPaths(t, dest)
}

// A rename out of the output can fail with EXDEV although the device numbers
// match: two mounts of one filesystem, or an output that is a mount of its
// own. The emptying then moves the entries into a directory beside the output,
// and when that one is across a mount too, into a hidden directory inside the
// output, which it removes. Either way the output ends up holding the kept
// paths alone, and nothing is left in any of the three places. A test cannot
// mount, so the renames are made to fail the way the kernel fails them.
func TestEmptyTaskOutput_CrossMountFallsBackBesideThenInside(t *testing.T) {
	for name, tc := range map[string]struct {
		crossed func(holder, stagingRoot, parent string) bool
		landsIn func(stagingRoot, parent, dest string) string
	}{
		"staging root across a mount": {
			crossed: func(holder, stagingRoot, _ string) bool { return holder == stagingRoot },
			landsIn: func(_, parent, _ string) string { return parent },
		},
		"output is a mount of its own": {
			crossed: func(holder, stagingRoot, parent string) bool { return holder == stagingRoot || holder == parent },
			landsIn: func(_, _, dest string) string { return dest },
		},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "gen")
			stagingRoot := filepath.Join(t.TempDir(), "scratch")
			writeTree(t, dest, map[string]string{
				"stale.html":                "stale",
				"public/static/old.html":    "stale",
				"migration-bundle/0001.sql": "ceded",
				"nested/keep/a.txt":         "preserved",
				"nested/stale.txt":          "stale",
			})
			kept := snapshotInodes(t, dest, ".", "migration-bundle", "migration-bundle/0001.sql",
				"nested", "nested/keep", "nested/keep/a.txt")
			asked := failStaleRenames(t, func(holder string) error {
				if tc.crossed(holder, stagingRoot, parent) {
					return syscall.EXDEV
				}
				return nil
			})

			if err := EmptyTaskOutput(dest, stagingRoot, "migration-bundle", "nested/keep"); err != nil {
				t.Fatalf("EmptyTaskOutput: %v", err)
			}

			assertPaths(t, dest,
				"migration-bundle", "migration-bundle/0001.sql",
				"nested", "nested/keep", "nested/keep/a.txt",
			)
			assertSameInodes(t, dest, kept)
			assertNothingBeside(t, dest)
			assertEmptyDir(t, stagingRoot)
			if asked[stagingRoot] == 0 {
				t.Error("no rename was tried into the staging root first")
			}
			// Three owned entries: stale.html, public and nested/stale.txt.
			if landed := tc.landsIn(stagingRoot, parent, dest); asked[landed] != 3 {
				t.Errorf("renames into %s = %d, want the 3 owned entries (asked: %v)", landed, asked[landed], asked)
			}
		})
	}
}

// When every place a rename could land is across a mount, the emptying fails
// with the kernel's error instead of leaving the entries and reporting success.
func TestEmptyTaskOutput_CrossMountEverywhereFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"stale.html": "stale"})
	failStaleRenames(t, func(string) error { return syscall.EXDEV })

	err := EmptyTaskOutput(dest, filepath.Join(t.TempDir(), "scratch"))
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("EmptyTaskOutput = %v, want EXDEV", err)
	}
	if !strings.Contains(err.Error(), "empty output directory") {
		t.Errorf("error %q does not say what failed", err)
	}
	assertPaths(t, dest, "stale.html")
	assertNothingBeside(t, dest)
}

// An error other than EXDEV is not answered by a fallback: the entries are
// tried once, into the first place, and the failure is returned.
func TestEmptyTaskOutput_OtherErrorsAreNotRetriedElsewhere(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "gen")
	stagingRoot := filepath.Join(t.TempDir(), "scratch")
	writeTree(t, dest, map[string]string{"stale.html": "stale"})
	asked := failStaleRenames(t, func(string) error { return syscall.EACCES })

	err := EmptyTaskOutput(dest, stagingRoot)
	if err == nil || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("EmptyTaskOutput = %v, want EACCES", err)
	}
	if asked[stagingRoot] != 1 || asked[parent] != 0 || asked[dest] != 0 {
		t.Errorf("renames asked = %v, want one into the staging root and none elsewhere", asked)
	}
	assertPaths(t, dest, "stale.html")
	assertNothingBeside(t, dest)
	assertEmptyDir(t, stagingRoot)
}
