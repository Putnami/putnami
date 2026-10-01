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
