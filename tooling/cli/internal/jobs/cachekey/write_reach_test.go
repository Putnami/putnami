package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/store"
)

func writeSelectionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A write reaches a selection only when it can touch a file the patterns
// select: an exclusion that covers the written tree, or a file the include
// does not match, leaves the selection unreached although the region overlaps.
func TestFileSelectionReachesOnlyWhatItsPatternsSelect(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	at := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	var scanner selectionScanner
	selection := scanner.Select(dir, []string{"**/*.go", "!**/*_test.go", "!**/.gen/**", "go.mod"})
	cases := []struct {
		name  string
		write plannedWrite
		want  bool
	}{
		{"an excluded tree", plannedWrite{Path: at(".gen/conf"), Tree: true}, false},
		{"an excluded nested tree", plannedWrite{Path: at("pkg/.gen"), Tree: true}, false},
		{"a file no include matches", plannedWrite{Path: at("schema/openapi.json")}, false},
		{"an excluded file", plannedWrite{Path: at("pkg/a_test.go")}, false},
		{"a selected file", plannedWrite{Path: at("pkg/a.go")}, true},
		{"a literal file", plannedWrite{Path: at("go.mod")}, true},
		{"a tree that can hold a source", plannedWrite{Path: at("pkg"), Tree: true}, true},
		{"the directory itself", plannedWrite{Path: dir, Tree: true}, true},
		{"an ancestor", plannedWrite{Path: filepath.Dir(dir), Tree: true}, true},
		{"another project", plannedWrite{Path: filepath.Join(filepath.Dir(dir), "other"), Tree: true}, false},
	}
	for _, testCase := range cases {
		if got := selection.Reaches(testCase.write); got != testCase.want {
			t.Errorf("%s: Reaches(%+v) = %v, want %v", testCase.name, testCase.write, got, testCase.want)
		}
		if testCase.want && !selection.Region().Overlaps(testCase.write.Region()) {
			t.Errorf("%s: reached outside the region", testCase.name)
		}
	}
	if !selection.Widened().Reaches(plannedWrite{Path: at(".gen/conf"), Tree: true}) {
		t.Error("a widened selection is bounded by its region alone")
	}
}

// A Go embed selector reads the Go sources outside every directory the Go
// tool skips and the files its embed directives name. A write elsewhere in
// the project, under .gen included, does not reach it.
func TestFileSelectionBoundsAGoEmbedSelectorByItsDirectives(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	at := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	writeSelectionFile(t, at("go.mod"), "module example.com/proj\n")
	writeSelectionFile(t, at("version.go"), "package proj\nimport _ \"embed\"\n//go:embed .gen/version.json\nvar version []byte\n")
	writeSelectionFile(t, at(".gen/version.json"), "{}")
	var scanner selectionScanner
	selection := scanner.Select(dir, []string{"go-embed:build"})
	cases := []struct {
		name       string
		write      plannedWrite
		reaches    bool
		directives bool
	}{
		{"a tree beside the embedded file", plannedWrite{Path: at(".gen/conf"), Tree: true}, false, false},
		{"a file beside it", plannedWrite{Path: at(".gen/schema.json")}, false, false},
		{"the embedded file", plannedWrite{Path: at(".gen/version.json")}, true, false},
		{"a tree holding it", plannedWrite{Path: at(".gen"), Tree: true}, true, false},
		{"a module file that can reject it", plannedWrite{Path: at(".gen/go.mod")}, true, true},
		{"a new source", plannedWrite{Path: at("pkg/new.go")}, true, true},
		{"a tree that can hold a source", plannedWrite{Path: at("pkg"), Tree: true}, true, true},
		{"a file the scan ignores", plannedWrite{Path: at("testdata/x.go")}, false, false},
		{"a file that is no source", plannedWrite{Path: at("README.md")}, false, false},
	}
	for _, testCase := range cases {
		if got := selection.Reaches(testCase.write); got != testCase.reaches {
			t.Errorf("%s: Reaches = %v, want %v", testCase.name, got, testCase.reaches)
		}
		if got := selection.ReachesDirectives(testCase.write); got != testCase.directives {
			t.Errorf("%s: ReachesDirectives = %v, want %v", testCase.name, got, testCase.directives)
		}
	}
	if !selection.ReachesDirectivesIn(store.PathsRegion([]string{at("pkg")})) {
		t.Error("a region in the project can change the directives")
	}
}

// A pattern set the selection cannot narrow is bounded by its region: no
// pattern, a Git candidate pattern, and a Go embed selector whose sources
// include a link or whose directives do not parse.
func TestFileSelectionKeepsTheRegionItCannotNarrow(t *testing.T) {
	root := t.TempDir()
	linked := filepath.Join(root, "linked")
	writeSelectionFile(t, filepath.Join(linked, "shared.go.txt"), "package linked\n")
	if err := os.Symlink("shared.go.txt", filepath.Join(linked, "a.go")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken")
	writeSelectionFile(t, filepath.Join(broken, "a.go"), "package broken\nimport _ \"embed\"\n//go:embed ../escape\nvar x []byte\n")
	plain := filepath.Join(root, "plain")
	var scanner selectionScanner
	for name, selection := range map[string]fileSelection{
		"no pattern":      scanner.Select(plain, nil),
		"a git pattern":   scanner.Select(plain, []string{"git:**/*.go", "!**/.gen/**"}),
		"a source link":   scanner.Select(linked, []string{"go-embed:build"}),
		"a bad directive": scanner.Select(broken, []string{"go-embed:build"}),
		"a region":        regionSelection(store.PathsRegion([]string{filepath.Join(plain, ".gen")})),
		"an unknown kind": scanner.Select(plain, []string{"go-embed:other", "!**/.gen/**"}),
	} {
		dir := plain
		switch name {
		case "a source link":
			dir = linked
		case "a bad directive":
			dir = broken
		}
		if !selection.Reaches(plannedWrite{Path: filepath.Join(dir, ".gen", "conf"), Tree: true}) {
			t.Errorf("%s: a write in its region does not reach it", name)
		}
	}
}

// A write the digest may read under another name than the one it declares is
// decided by region: a file the selection reads through a link, a literal
// pattern segment in another case, a path whose existing segments are spelled
// otherwise on disk, and a pattern the store cleans before it globs.
func TestFileSelectionDecidesByRegionWhatItCannotSpell(t *testing.T) {
	var dir string
	at := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	// Each case starts from its own tree, so a link one case adds never
	// decides another.
	fixture := func(t *testing.T) {
		dir = filepath.Join(t.TempDir(), "proj")
		writeSelectionFile(t, at(".gen/version.json"), "{}\n")
		writeSelectionFile(t, at("Config/app.json"), "{}\n")
	}

	t.Run("a selected link", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		patterns := []string{"**/*.json", "!**/.gen/**"}
		write := plannedWrite{Path: at(".gen/version.json")}
		if scanner.Select(dir, patterns).Reaches(write) {
			t.Fatal("an excluded file reached a selection that reads no link")
		}
		if err := os.MkdirAll(at("src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", ".gen", "version.json"), at("src/version.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		var fresh selectionScanner
		if !fresh.Select(dir, patterns).Reaches(write) {
			t.Fatal("a write to the target of a selected link did not reach the selection")
		}
	})

	t.Run("a literal segment in another case", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		if !scanner.Select(dir, []string{"Config/*.yaml"}).Reaches(plannedWrite{Path: at("config/app.json")}) {
			t.Fatal("a write that spells the pattern's literal segment in another case was decided by pattern")
		}
		if scanner.Select(dir, []string{"Config/*.yaml"}).Reaches(plannedWrite{Path: at("Config/app.json")}) {
			t.Fatal("a write spelled as the pattern is, that the pattern does not match, reached the selection")
		}
	})

	t.Run("a path spelled otherwise on disk", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		write := plannedWrite{Path: at("config/new.json")}
		_, err := os.Lstat(at("config"))
		caseInsensitive := err == nil
		if got := scanner.Select(dir, []string{"**/*.json", "!config/**"}).Reaches(write); got != caseInsensitive {
			t.Fatalf("Reaches = %v, want %v: the write lands in Config on a case-insensitive filesystem only", got, caseInsensitive)
		}
	})

	t.Run("a pattern not in clean form", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		if !scanner.Select(dir, []string{"Config/./*.yaml"}).Reaches(plannedWrite{Path: at("Config/app.json")}) {
			t.Fatal("a pattern the store cleans was decided by matching it as written")
		}
	})
}
