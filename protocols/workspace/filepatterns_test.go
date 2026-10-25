package workspace

import "testing"

// TestSelectsPath pins the grammar both readers of `filePatterns` share: the
// build cache's file hasher and `--impacted`'s project selection. The cases
// below are the forms real declarations use — an input under the project root,
// one reaching outside it with "../", a recursive "**", and an exclusion.
func TestSelectsPath(t *testing.T) {
	patterns := []string{
		"doc/**", "internal/cli/testdata/**", "**/*.go", "!**/*_test.go",
		"putnami.json", "../../**/putnami.json", "../../LICENSE.md", "../../.github/**",
		"../contributor/src/**",
	}
	for _, testCase := range []struct {
		rel  string
		want bool
	}{
		{"doc/reports/release-rehearsal.md", true},
		{"internal/cli/testdata/release-plan.json", true},
		{"internal/cli/app.go", true},
		{"internal/cli/app_test.go", false},
		{"putnami.json", true},
		{"../../typescript/framework/utils/putnami.json", true},
		{"../../LICENSE.md", true},
		{"../../GOVERNANCE.md", false},
		{"../../.github/workflows/ci.yml", true},
		{"../contributor/src/skills/fix/SKILL.md", true},
		{"../memory-store/src/skills/fix/SKILL.md", false},
		{"scripts/install.sh", false},
	} {
		if got := SelectsPath(testCase.rel, patterns); got != testCase.want {
			t.Errorf("SelectsPath(%q) = %v, want %v", testCase.rel, got, testCase.want)
		}
	}
}

// A wildcard of an ordinary include never matches a ".." segment, because the
// collector lists files under the pattern's static prefix and a directory never
// lists "..". Only a literal ".." reaches outside the project root. A "git:"
// include and an exclusion keep matching ".." with a wildcard.
func TestSelectsPath_OrdinaryWildcardStaysInsideItsRoot(t *testing.T) {
	for _, testCase := range []struct {
		rel      string
		patterns []string
		want     bool
	}{
		{"own.go", []string{"**"}, true},
		{"conf/env.prod.yaml", []string{"**"}, true},
		{"../../docs/README.md", []string{"**"}, false},
		{"../../docs/README.md", []string{"**/*.md"}, false},
		{"internal/testdata/a.json", []string{"**/testdata/**"}, true},
		{"../../identity/testdata/a.json", []string{"**/testdata/**"}, false},
		{"../x.json", []string{"*/x.json"}, false},
		{"../x.json", []string{"../*.json"}, true},
		{"../shared/a/b.go", []string{"../shared/**"}, true},
		{"../../other/b.go", []string{"../**"}, false},
		{"../../putnami.json", []string{"../../**/putnami.json"}, true},
		{"../../docs/README.md", []string{"git:**"}, true},
		{"../shared/gen/a.go", []string{"../shared/**", "!**/gen/**"}, false},
	} {
		if got := SelectsPath(testCase.rel, testCase.patterns); got != testCase.want {
			t.Errorf("SelectsPath(%q, %q) = %v, want %v", testCase.rel, testCase.patterns, got, testCase.want)
		}
	}
	// MatchFilePattern is the exclusion grammar and is unchanged.
	if !MatchFilePattern("../../docs/README.md", "**") {
		t.Error(`MatchFilePattern("../../docs/README.md", "**") = false, want true`)
	}
}

// An empty include list selects nothing here. The cache hasher reads "no
// patterns" as "every file under the project root", but that is a statement
// about a directory: answering true for an arbitrary path would make every
// project a reader of every file under --impacted.
func TestSelectsPath_EmptyPatternSetSelectsNothing(t *testing.T) {
	if SelectsPath("src/main.go", nil) {
		t.Error("SelectsPath with no patterns = true, want false")
	}
	if SelectsPath("src/main.go", []string{"!src/**"}) {
		t.Error("SelectsPath with only exclusions = true, want false")
	}
}

func TestSplitFilePatterns(t *testing.T) {
	includes, excludes := SplitFilePatterns([]string{"src/**", "!src/gen/**", "doc/*.md"})
	if len(includes) != 2 || includes[0] != "src/**" || includes[1] != "doc/*.md" {
		t.Errorf("includes = %v, want [src/** doc/*.md]", includes)
	}
	if len(excludes) != 1 || excludes[0] != "src/gen/**" {
		t.Errorf("excludes = %v, want [src/gen/**] with the marker stripped", excludes)
	}
}

func TestGitCandidatePatternsUseTheSamePathGrammar(t *testing.T) {
	for _, path := range []string{"own.go", "../clientgen-extension/doc/new.md", "../../README.md", "../../deleted.txt"} {
		if !SelectsPath(path, []string{"git:**"}) {
			t.Errorf("Git candidate input did not select %q", path)
		}
	}
	patterns := []string{"git:../other/doc/**", "!git:../other/doc/private/**"}
	if !SelectsPath("../other/doc/new.md", patterns) || SelectsPath("../other/doc/private/secret.md", patterns) {
		t.Fatal("Git candidate include/exclusion grammar differs from ordinary patterns")
	}
}

// "**" matches zero segments as well as many, which is what makes
// "../../**/putnami.json" select a manifest sitting directly at the workspace
// root and not only ones nested under a directory.
func TestMatchFilePattern_DoubleStarMatchesZeroSegments(t *testing.T) {
	for _, testCase := range []struct {
		rel, pattern string
		want         bool
	}{
		{"../../putnami.json", "../../**/putnami.json", true},
		{"../../tooling/cli/putnami.json", "../../**/putnami.json", true},
		{"src/a/b/c.ts", "src/**", true},
		// Zero segments includes the prefix itself, so "src/**" matches "src".
		// The cache hasher stats every match and drops directories; a git diff
		// names files, so neither reader is troubled by it.
		{"src", "src/**", true},
		{"src/a.ts", "src/*", true},
		{"src/a/b.ts", "src/*", false},
	} {
		if got := MatchFilePattern(testCase.rel, testCase.pattern); got != testCase.want {
			t.Errorf("MatchFilePattern(%q, %q) = %v, want %v", testCase.rel, testCase.pattern, got, testCase.want)
		}
	}
}

// An exclusion covers a tree only when it removes every path that can lie
// there: each covered tree below is checked against paths written beneath it,
// and each uncovered one names a path the exclusions leave selected.
func TestExcludesTree(t *testing.T) {
	excludes := []string{"**/.gen/**", "!ignored", "**/*_test.go", "build/out/**", "../shared/**"}
	for _, testCase := range []struct {
		rel     string
		covered bool
		// beneath is a path at or under rel the exclusions leave selected
		// when the tree is not covered, or one they remove when it is.
		beneath string
	}{
		{".gen", true, ".gen/conf/.env.local.yaml"},
		{".gen/conf", true, ".gen/conf/.env.local.yaml"},
		{"pkg/.gen/schema", true, "pkg/.gen/schema/openapi.json"},
		{"build/out", true, "build/out/a/b.txt"},
		{"build/out/sub", true, "build/out/sub/x"},
		{"../shared/lib", true, "../shared/lib/a.go"},
		{"build", false, "build/main.go"},
		{"generated", false, "generated/client.go"},
		{"pkg", false, "pkg/a.go"},
		{"x_test.go", false, "x_test.go/inner.go"},
		{".", false, "a.go"},
	} {
		if got := ExcludesTree(testCase.rel, excludes); got != testCase.covered {
			t.Errorf("ExcludesTree(%q) = %v, want %v", testCase.rel, got, testCase.covered)
		}
		if got := MatchesAnyFilePattern(testCase.beneath, excludes); got != testCase.covered {
			t.Errorf("the exclusions match %q: %v, want %v", testCase.beneath, got, testCase.covered)
		}
	}
	if !ExcludesTree("anything/at/all", []string{"**"}) {
		t.Error(`"**" excludes every tree`)
	}
	if ExcludesTree("a", nil) {
		t.Error("no exclusion covers a tree")
	}
}
