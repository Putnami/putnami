package sdd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const migrationRecipeIndex = `{
  "protocolVersion": 1,
  "recipes": [
    {
      "intention": "Add a database migration",
      "sample": "go/samples/migrations-feature",
      "primitives": ["go.putnami.dev/migratecli: Run"],
      "antiPatterns": ["A hand-rolled migrations table"],
      "since": "0.0.0-20260917065345-3cc91b658"
    }
  ]
}`

// TestRecipesAbsentIndexIsAdoption keeps a workspace without samples green.
func TestRecipesAbsentIndexIsAdoption(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "recipe-indexes-are-checked", "an-absent-index-is-adoption")
	report, err := BuildRecipesResult(fixtureWorkspace("w", decisionsWorkspace(t)))
	if err != nil {
		t.Fatalf("an absent index failed the task: %v", err)
	}
	if len(report.Indexes) != 0 || report.Recipes != 0 {
		t.Fatalf("report = %+v, want nothing read", report)
	}
}

// TestRecipesResolvingIndexPasses keeps the gate from being vacuously red.
func TestRecipesResolvingIndexPasses(t *testing.T) {
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "go", "samples", RecipeIndexFilename), migrationRecipeIndex)
	if err := os.MkdirAll(filepath.Join(root, "go", "samples", "migrations-feature"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := BuildRecipesResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("a resolving index failed the task: %v (%+v)", err, report.Diagnostics)
	}
	if report.Recipes != 1 || len(report.Indexes) != 1 || report.Indexes[0] != "go/samples/recipes.json" {
		t.Fatalf("report = %+v, want one recipe in go/samples/recipes.json", report)
	}
}

// TestRecipesDanglingSampleFailsNamingIt is the acceptance line for a
// recipe pointing at a sample that does not exist: it fails validate.
func TestRecipesDanglingSampleFailsNamingIt(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "recipe-indexes-are-checked", "a-dangling-sample-fails-naming-it")
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "go", "samples", RecipeIndexFilename), migrationRecipeIndex)
	// A file is not a sample directory.
	writeFixtureFile(t, filepath.Join(root, "go", "samples", "migrations-feature"), "not a directory")

	report, err := BuildRecipesResult(fixtureWorkspace("w", root))
	if err == nil {
		t.Fatal("a dangling sample did not fail the task")
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
	}
	finding := report.Diagnostics[0]
	if finding.Code != ErrorCodeRecipeSampleMissing || finding.Field != "go/samples/recipes.json#recipes[0].sample" ||
		!strings.Contains(finding.Message, "go/samples/migrations-feature") {
		t.Fatalf("finding = %+v, want the missing sample named on its recipe", finding)
	}
}

// TestRecipesDuplicateIntentionFails pins "one intention, one recipe".
func TestRecipesDuplicateIntentionFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "recipe-indexes-are-checked", "a-duplicate-intention-fails")
	root := decisionsWorkspace(t)
	entry := `{"intention":"%s","sample":"typescript/samples/06-database","primitives":["p"],"antiPatterns":["a"],"since":"1"}`
	index := `{"protocolVersion":1,"recipes":[` +
		strings.Replace(entry, "%s", "Add a database migration", 1) + `,` +
		strings.Replace(entry, "%s", "add a database migration ", 1) + `]}`
	writeFixtureFile(t, filepath.Join(root, "typescript", "samples", RecipeIndexFilename), index)
	if err := os.MkdirAll(filepath.Join(root, "typescript", "samples", "06-database"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := BuildRecipesResult(fixtureWorkspace("w", root))
	if err == nil {
		t.Fatal("two recipes for one intention did not fail the task")
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Field != "typescript/samples/recipes.json#recipes[1].intention" {
		t.Fatalf("diagnostics = %+v, want the second recipe's intention refused", report.Diagnostics)
	}
}

// TestRecipesMalformedIndexFailsClosed covers the shape rules.
func TestRecipesMalformedIndexFailsClosed(t *testing.T) {
	for name, contents := range map[string]string{
		"not json":         `{"protocolVersion":1,`,
		"unknown member":   `{"protocolVersion":1,"recipes":[],"mode":"report"}`,
		"wrong version":    `{"protocolVersion":2,"recipes":[]}`,
		"empty intention":  `{"protocolVersion":1,"recipes":[{"intention":" ","sample":"a","primitives":["p"],"antiPatterns":["a"],"since":"1"}]}`,
		"two-line since":   `{"protocolVersion":1,"recipes":[{"intention":"i","sample":"a","primitives":["p"],"antiPatterns":["a"],"since":"1\n2"}]}`,
		"escaping sample":  `{"protocolVersion":1,"recipes":[{"intention":"i","sample":"../a","primitives":["p"],"antiPatterns":["a"],"since":"1"}]}`,
		"no primitives":    `{"protocolVersion":1,"recipes":[{"intention":"i","sample":"a","primitives":[],"antiPatterns":["a"],"since":"1"}]}`,
		"long antiPattern": `{"protocolVersion":1,"recipes":[{"intention":"i","sample":"a","primitives":["p"],"antiPatterns":["` + strings.Repeat("x", recipeLineMaxLength+1) + `"],"since":"1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := decisionsWorkspace(t)
			writeFixtureFile(t, filepath.Join(root, "python", "samples", RecipeIndexFilename), contents)
			if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
			report, err := BuildRecipesResult(fixtureWorkspace("w", root))
			if err == nil {
				t.Fatalf("a malformed index passed: %+v", report)
			}
			for _, finding := range report.Diagnostics {
				if finding.Code != ErrorCodeInvalidRecipeIndex {
					t.Fatalf("finding %+v, want only %s", finding, ErrorCodeInvalidRecipeIndex)
				}
			}
		})
	}
}
