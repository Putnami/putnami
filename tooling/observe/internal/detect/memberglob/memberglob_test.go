package memberglob

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSegmentsRefuseAPatternOutsideTheRepository(t *testing.T) {
	for pattern, want := range map[string]bool{"a/**": true, "./a/": true, "crates/*": true, "": false, ".": false, "a//b": false, "../a": false, "/abs": false, "a/[": false} {
		if _, ok := Segments(pattern); ok != want {
			t.Errorf("Segments(%q) = %v, want %v", pattern, ok, want)
		}
	}
}

func TestDirsMatchEachPatternOnce(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"crates/api", "crates/worker/nested", "libs/a/b", "target/crates/x", "other"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var patterns [][]string
	for _, p := range []string{"crates/*", "libs/**", "crates/api"} {
		s, _ := Segments(p)
		patterns = append(patterns, s)
	}
	dirs, bounded := Dirs(root, patterns, func(name string) bool { return name == "target" }, 1000)
	want := []string{"crates/api", "crates/worker", "libs", "libs/a", "libs/a/b"}
	if bounded || !slices.Equal(dirs, want) {
		t.Errorf("Dirs = %v (bounded %v), want %v", dirs, bounded, want)
	}
	if _, bounded := Dirs(root, patterns, func(string) bool { return false }, 2); !bounded {
		t.Error("a walk past its bound is not reported")
	}
	if !Match(strings.Split("**/x", "/"), []string{"a", "b", "x"}) || Match([]string{"a"}, []string{"a", "b"}) {
		t.Error("Match")
	}
}
