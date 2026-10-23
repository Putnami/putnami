package cachekey

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
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
	selection := scanner.Select(dir, []string{"**/*.go", "!**/*_test.go", "!**/.gen/**", "go.mod"}, nil)
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

// fixedExpander expands every selector to one expansion, or fails with err.
type fixedExpander struct {
	expansion store.InputExpansion
	err       error
}

func (e fixedExpander) ExpandInput(string, string) (store.InputExpansion, error) {
	return e.expansion, e.err
}

func (e fixedExpander) Identity() string { return fmt.Sprintf("%v|%v", e.expansion, e.err) }

// An input selector reads the files its answer lists and depends on the paths
// its basis selects. A write elsewhere in the project does not reach it, and
// a write a content or a listing set of its basis selects can change the
// answer.
func TestFileSelectionBoundsASelectorByItsAnswerAndBasis(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	at := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	writeSelectionFile(t, at("src/main.go"), "package proj\n")
	writeSelectionFile(t, at("assets/a.txt"), "A")
	expander := fixedExpander{expansion: store.InputExpansion{
		Files: []string{"assets/a.txt", "src/main.go"},
		Basis: wsproto.InputBasis{Content: [][]string{{"src/*.go", "!src/*_test.go"}}, Listing: [][]string{{"assets/*.txt"}}},
	}}
	var scanner selectionScanner
	selection := scanner.Select(dir, []string{"sel:build"}, expander)
	cases := []struct {
		name    string
		write   plannedWrite
		reaches bool
		basis   bool
	}{
		{"a tree no file or set holds", plannedWrite{Path: at("gen/conf"), Tree: true}, false, false},
		{"a file beside the listed file", plannedWrite{Path: at("assets/b.json")}, false, false},
		{"the listed file", plannedWrite{Path: at("assets/a.txt")}, true, true},
		{"a file a listing set selects", plannedWrite{Path: at("assets/new.txt")}, true, true},
		{"a tree holding the listed file", plannedWrite{Path: at("assets"), Tree: true}, true, true},
		{"a file a content set selects", plannedWrite{Path: at("src/new.go")}, true, true},
		{"a file a content set excludes", plannedWrite{Path: at("src/a_test.go")}, false, false},
		{"a tree a content set can select in", plannedWrite{Path: at("src/pkg"), Tree: true}, true, true},
		{"a file outside every set", plannedWrite{Path: at("README.md")}, false, false},
	}
	for _, testCase := range cases {
		if got := selection.Reaches(testCase.write); got != testCase.reaches {
			t.Errorf("%s: Reaches = %v, want %v", testCase.name, got, testCase.reaches)
		}
		if got := selection.ReachesBasis(testCase.write); got != testCase.basis {
			t.Errorf("%s: ReachesBasis = %v, want %v", testCase.name, got, testCase.basis)
		}
	}
	if !selection.ReachesBasisIn(store.PathsRegion([]string{at("src")})) {
		t.Error("a region in the project can change the basis")
	}
	failed := scanner.Select(filepath.Join(t.TempDir(), "failed"), []string{"sel:build"}, fixedExpander{err: store.ErrInputSelector})
	write := plannedWrite{Path: filepath.Join(failed.dir, "README.md")}
	if !failed.Reaches(write) || !failed.ReachesBasis(write) {
		t.Error("a failed expansion is bounded by less than its region")
	}
}

// A pattern set the selection cannot narrow is bounded by its region: no
// pattern, a Git candidate pattern, an excluded selector, and a selector that
// does not expand.
func TestFileSelectionKeepsTheRegionItCannotNarrow(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	var scanner selectionScanner
	for name, selection := range map[string]fileSelection{
		"no pattern":           scanner.Select(plain, nil, nil),
		"a git pattern":        scanner.Select(plain, []string{"git:**/*.go", "!**/.gen/**"}, nil),
		"an excluded selector": scanner.Select(plain, []string{"**/*.go", "!sel:build"}, nil),
		"no expander":          scanner.Select(plain, []string{"sel:build", "!**/.gen/**"}, nil),
		"a failed expansion":   scanner.Select(plain, []string{"sel:build"}, fixedExpander{err: store.ErrInputSelector}),
		"a region":             regionSelection(store.PathsRegion([]string{filepath.Join(plain, ".gen")})),
	} {
		if !selection.Reaches(plannedWrite{Path: filepath.Join(plain, ".gen", "conf"), Tree: true}) {
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
		if scanner.Select(dir, patterns, nil).Reaches(write) {
			t.Fatal("an excluded file reached a selection that reads no link")
		}
		if err := os.MkdirAll(at("src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", ".gen", "version.json"), at("src/version.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		var fresh selectionScanner
		if !fresh.Select(dir, patterns, nil).Reaches(write) {
			t.Fatal("a write to the target of a selected link did not reach the selection")
		}
	})

	t.Run("a literal segment in another case", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		if !scanner.Select(dir, []string{"Config/*.yaml"}, nil).Reaches(plannedWrite{Path: at("config/app.json")}) {
			t.Fatal("a write that spells the pattern's literal segment in another case was decided by pattern")
		}
		if scanner.Select(dir, []string{"Config/*.yaml"}, nil).Reaches(plannedWrite{Path: at("Config/app.json")}) {
			t.Fatal("a write spelled as the pattern is, that the pattern does not match, reached the selection")
		}
	})

	t.Run("a path spelled otherwise on disk", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		write := plannedWrite{Path: at("config/new.json")}
		_, err := os.Lstat(at("config"))
		caseInsensitive := err == nil
		if got := scanner.Select(dir, []string{"**/*.json", "!config/**"}, nil).Reaches(write); got != caseInsensitive {
			t.Fatalf("Reaches = %v, want %v: the write lands in Config on a case-insensitive filesystem only", got, caseInsensitive)
		}
	})

	t.Run("a pattern not in clean form", func(t *testing.T) {
		fixture(t)
		var scanner selectionScanner
		if !scanner.Select(dir, []string{"Config/./*.yaml"}, nil).Reaches(plannedWrite{Path: at("Config/app.json")}) {
			t.Fatal("a pattern the store cleans was decided by matching it as written")
		}
	})
}
