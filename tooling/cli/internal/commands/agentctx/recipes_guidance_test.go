package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

const seededGoRecipes = `{
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

// writeBuildExtension creates a local extension whose agent content carries
// the build skill, and returns the config that opts into it.
func writeBuildExtension(t *testing.T, root string) *wsproto.Config {
	t.Helper()
	writeLocalContentExtension(t, root, "/tools/workflows", "@local/workflows", "build", "Read the recipes.\n")
	return localContentConfig("/tools/workflows", "@local/workflows")
}

// TestRenderRecipeReferenceRendersEveryIndex pins the rendered shape: one
// section per language, in the declared order, each recipe with its sample,
// version, primitives and anti-patterns.
func TestRenderRecipeReferenceRendersEveryIndex(t *testing.T) {
	root := t.TempDir()
	writePathFile(t, root, "go/samples/recipes.json", seededGoRecipes)
	writePathFile(t, root, "typescript/samples/recipes.json",
		strings.ReplaceAll(strings.ReplaceAll(seededGoRecipes, "go/samples/migrations-feature", "typescript/samples/06-database"),
			"go.putnami.dev/migratecli: Run", "@putnami/database: sqlSourceInline"))

	rendered := renderRecipeReference(root)
	for _, want := range []string{
		"# Putnami recipes\n",
		"\n## TypeScript\n\n### Add a database migration\n\n- Sample: `typescript/samples/06-database`\n",
		"\n## Go\n\n### Add a database migration\n\n- Sample: `go/samples/migrations-feature`\n",
		"- Since: `0.0.0-20260917065345-3cc91b658`\n- Primitives:\n  - `go.putnami.dev/migratecli: Run`\n- Replaces:\n  - A hand-rolled migrations table\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered reference is missing %q:\n%s", want, rendered)
		}
	}
	if strings.Index(rendered, "## TypeScript") > strings.Index(rendered, "## Go") {
		t.Fatalf("languages are not rendered in the declared order:\n%s", rendered)
	}
	if strings.Contains(rendered, "## Python") {
		t.Fatalf("a language without an index rendered a section:\n%s", rendered)
	}
}

// TestRenderRecipeReferenceRendersNothingWithoutAUsableIndex keeps a consumer
// workspace, which has no samples, untouched, and leaves a bad index to
// recipes-validate rather than failing context generation.
func TestRenderRecipeReferenceRendersNothingWithoutAUsableIndex(t *testing.T) {
	for name, contents := range map[string]string{
		"absent":         "",
		"not json":       `{"protocolVersion":1,`,
		"unknown member": `{"protocolVersion":1,"recipes":[],"extra":true}`,
		"wrong version":  `{"protocolVersion":2,"recipes":[{"intention":"i"}]}`,
		"no recipe":      `{"protocolVersion":1,"recipes":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if contents != "" {
				writePathFile(t, root, "go/samples/recipes.json", contents)
			}
			if rendered := renderRecipeReference(root); rendered != "" {
				t.Fatalf("rendered %q, want nothing", rendered)
			}
			changed, err := writeRecipeReferences(root, writeBuildExtension(t, root))
			if err != nil || changed {
				t.Fatalf("changed = %v, err = %v; want no write", changed, err)
			}
		})
	}
}

func TestRecipeReferenceWritesOnlyBuild(t *testing.T) {
	root := t.TempDir()
	writePathFile(t, root, "go/samples/recipes.json", seededGoRecipes)
	cfg := writeBuildExtension(t, root)
	writePathFile(t, root, "tools/workflows/src/skills/plan/SKILL.md", "Legacy planning entry.\n")
	writePathFile(t, root, "tools/workflows/src/skills/plan/references/recipes.md", "Keep this legacy file untouched.\n")
	changed, err := writeRecipeReferences(root, cfg)
	if err != nil || !changed {
		t.Fatalf("write build recipes changed=%v, err=%v", changed, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "tools/workflows/src/skills/plan/references/recipes.md")); err != nil || string(got) != "Keep this legacy file untouched.\n" {
		t.Fatalf("legacy plan reference was rewritten: %q, %v", got, err)
	}
}

// TestContextGenerateCarriesTheRecipeIndexToTheHostCopy proves the whole path:
// a committed index reaches the build skill's reference asset in
// the in-tree source and the materialized `.agents` copy in one run, and a
// second run changes nothing.
func TestContextGenerateCarriesTheRecipeIndexToTheHostCopy(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "canonical-consumer-migration", "canonical-recipe-materialization")
	root := t.TempDir()
	writePathFile(t, root, "go/samples/recipes.json", seededGoRecipes)
	cfg := writeBuildExtension(t, root)

	first, err := ContextGenerateWithResult(root, cfg, nil)
	if err != nil {
		t.Fatalf("context generate: %v", err)
	}
	if !first.AgentWorkflowsChanged {
		t.Fatal("the first generation reported no change")
	}
	want := renderRecipeReference(root)
	for _, rel := range []string{
		"tools/workflows/src/skills/build/references/recipes.md",
		".agents/skills/build/references/recipes.md",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s =\n%s\nwant\n%s", rel, got, want)
		}
	}

	second, err := ContextGenerateWithResult(root, cfg, nil)
	if err != nil {
		t.Fatalf("second context generate: %v", err)
	}
	if second.AgentWorkflowsChanged {
		t.Fatal("a second generation over unchanged indexes rewrote a file")
	}
}

// TestCommittedRecipeReferenceMatchesTheSampleIndexes is the drift gate for
// this repository: the build skill's committed reference is exactly what
// `putnami context generate` renders from the committed indexes.
func TestCommittedRecipeReferenceMatchesTheSampleIndexes(t *testing.T) {
	root := driftRepositoryRoot(t)
	want := renderRecipeReference(root)
	if want == "" {
		t.Fatal("this repository commits no usable recipe index, so this gate proved nothing")
	}
	path := filepath.Join(root, "tooling", "contributor", "src", "skills", recipeReferenceSkill, filepath.FromSlash(recipeReferenceAsset))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed recipe reference: %v", err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale; run `./putnamiw context generate` and commit the result", path)
	}
}
