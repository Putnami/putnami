package extension

import (
	"reflect"
	"testing"
)

// unsafePaths are the three classes every contract surface must reject,
// whatever it is naming: an absolute path ignores the root it was declared
// against, an escape leaves the tree the root was chosen to bound, and a
// backslash is a second spelling of a path the contract compares as text.
var unsafePaths = []string{
	"",
	"   ",
	".",
	"./",
	"..",
	"../shared",
	"a/../..",
	"/abs/path",
	"C:/abs/path",
	"C:drive-relative",
	"dist\\bin",
}

func TestNormalizeRelativePath(t *testing.T) {
	valid := map[string]string{
		".gen":                  ".gen",
		"bin/":                  "bin",
		"./bin/sample":          "bin/sample",
		"bin//sample":           "bin/sample",
		"a/b/../c":              "a/c",
		"clients/typescript/v1": "clients/typescript/v1",
	}
	for input, want := range valid {
		got, err := NormalizeRelativePath(input)
		if err != nil {
			t.Errorf("NormalizeRelativePath(%q) unexpected error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeRelativePath(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := append(append([]string{}, unsafePaths...),
		// A concrete path names one file, so a glob or a template variable is
		// a different kind of declaration, not a spelling of this one.
		"dist/**/*.js", "dist/*.map", "report-[0-9].json", "{projectRoot}/.gen",
	)
	for _, input := range invalid {
		if got, err := NormalizeRelativePath(input); err == nil {
			t.Errorf("NormalizeRelativePath(%q) = %q, want error", input, got)
		}
	}
}

func TestNormalizeInputPattern(t *testing.T) {
	valid := map[string]string{
		"cmd/**":          "cmd/**",
		"./cmd/**":        "cmd/**",
		"go.mod":          "go.mod",
		"tsconfig*.json":  "tsconfig*.json",
		"src/**/*.ts":     "src/**/*.ts",
		"report-[0-9].md": "report-[0-9].md",
		"node_modules/":   "node_modules",
	}
	for input, want := range valid {
		got, err := NormalizeInputPattern(input)
		if err != nil {
			t.Errorf("NormalizeInputPattern(%q) unexpected error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeInputPattern(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := append(append([]string{}, unsafePaths...),
		"{workspaceRoot}/go.mod", "[", "src/[abc",
	)
	for _, input := range invalid {
		if got, err := NormalizeInputPattern(input); err == nil {
			t.Errorf("NormalizeInputPattern(%q) = %q, want error", input, got)
		}
	}
}

// TestPathRulesAgree pins that the concrete and set forms differ in EXACTLY one
// dimension — globbing — so a surface cannot accidentally relax an escape or an
// absolute-path rule by choosing the other normalizer.
func TestPathRulesAgree(t *testing.T) {
	for _, unsafe := range unsafePaths {
		_, concreteErr := NormalizeRelativePath(unsafe)
		_, patternErr := NormalizeInputPattern(unsafe)
		if (concreteErr == nil) != (patternErr == nil) {
			t.Errorf("%q: concrete error = %v, pattern error = %v — the two rules disagree",
				unsafe, concreteErr, patternErr)
		}
	}
	if _, err := NormalizeRelativePath("cmd/**"); err == nil {
		t.Error("a concrete declaration must reject a glob")
	}
	if _, err := NormalizeInputPattern("cmd/**"); err != nil {
		t.Errorf("a pattern declaration must accept a glob: %v", err)
	}

	// The declared-output spelling is the concrete rule, not a copy of it.
	for _, input := range append(append([]string{}, unsafePaths...), "dist/*.map") {
		_, outputErr := NormalizeOutputPath(input)
		_, concreteErr := NormalizeRelativePath(input)
		if (outputErr == nil) != (concreteErr == nil) {
			t.Errorf("%q: NormalizeOutputPath and NormalizeRelativePath disagree", input)
		}
	}
}

func TestNormalizePatternList(t *testing.T) {
	if got := normalizePatternList(nil); got != nil {
		t.Errorf("normalizePatternList(nil) = %v, want nil", got)
	}
	got := normalizePatternList([]string{"internal/**", "./cmd/**", "go.mod", "../escape"})
	want := []string{"../escape", "cmd/**", "go.mod", "internal/**"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizePatternList = %v, want %v (invalid patterns kept as authored)", got, want)
	}

	// Normalizing must not write through a slice the caller still holds.
	original := []string{"b", "a"}
	normalized := normalizePatternList(original)
	if !reflect.DeepEqual(original, []string{"b", "a"}) {
		t.Errorf("caller slice was mutated: %v", original)
	}
	if !reflect.DeepEqual(normalized, []string{"a", "b"}) {
		t.Errorf("normalized = %v, want sorted", normalized)
	}
}
