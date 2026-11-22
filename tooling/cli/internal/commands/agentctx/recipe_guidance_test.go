package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The recipe line names the recipe of each language extension the workspace
// loads, and nothing for a workspace that loads neither.
func TestRecipeLineNamesTheRecipeOfEachLoadedLanguage(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-service-recipe", "the-guidance-links-the-recipe-of-each-loaded-language")
	goURL := "https://putnami.dev/docs/how-to/build-a-postgres-api-in-go"
	tsURL := "https://putnami.dev/docs/how-to/build-a-postgres-api-in-typescript"
	cases := []struct {
		name   string
		loaded map[string]bool
		want   []string
		absent []string
	}{
		{name: "neither", loaded: map[string]bool{"@putnami/rust": true}},
		{name: "go", loaded: map[string]bool{"@putnami/go": true}, want: []string{goURL + " (Go)"}, absent: []string{tsURL}},
		{name: "typescript", loaded: map[string]bool{"@putnami/typescript": true}, want: []string{tsURL + " (TypeScript)"}, absent: []string{goURL}},
		{name: "both", loaded: map[string]bool{"@putnami/go": true, "@putnami/typescript": true}, want: []string{goURL + " (Go) or " + tsURL + " (TypeScript)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := recipeLine(tc.loaded)
			if len(tc.want) == 0 {
				if line != "" {
					t.Fatalf("recipeLine = %q, want no line", line)
				}
				if guidanceBody("lint", "", line) != guidanceBody("lint", "", "") {
					t.Fatal("a workspace that loads no recipe language changed its block")
				}
				return
			}
			if !strings.HasPrefix(line, "- To build an HTTP API with Postgres, follow ") || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
				t.Fatalf("recipeLine = %q, want one guidance line", line)
			}
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("recipeLine = %q, want it to contain %q", line, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(line, absent) {
					t.Errorf("recipeLine = %q names %q, an extension the workspace does not load", line, absent)
				}
			}
			if body := guidanceBody("lint", "", line); !strings.Contains(body, line) || strings.Contains(body, recipePlaceholder) {
				t.Errorf("the block does not carry the recipe line:\n%s", body)
			}
		})
	}
}

// A workspace that loads a language extension
// gets its recipe line in the AGENTS.md that context generation writes.
func TestGeneratedGuidanceLinksTheRecipeOfTheDeclaredLanguage(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-service-recipe", "the-guidance-links-the-recipe-of-each-loaded-language")
	dir := t.TempDir()
	writeRecipeWorkspace(t, dir, "@putnami/go")
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatal(err)
	}
	got := readGuidance(t, dir, agentEntrypointPath)
	if want := "https://putnami.dev/docs/how-to/build-a-postgres-api-in-go (Go)"; !strings.Contains(got, want) {
		t.Fatalf("AGENTS.md does not link the Go recipe %q:\n%s", want, got)
	}
	if strings.Contains(got, "build-a-postgres-api-in-typescript") {
		t.Fatalf("AGENTS.md links the TypeScript recipe of a Go workspace:\n%s", got)
	}
}

// writeRecipeWorkspace writes a workspace that declares one extension by
// path, as a workspace that holds the extension's source does.
func writeRecipeWorkspace(t *testing.T, dir, name string) {
	t.Helper()
	config := `{"name":"shop","extensions":["/ext"]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	extDir := filepath.Join(dir, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"` + name + `","version":"0.1.0","cliContract":4,"commands":{}}` + "\n"
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Each recipe the line names is a page of this repository's documentation,
// which the documentation examples of the language check.
func TestEveryLinkedRecipeIsAPublishedPage(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-service-recipe", "every-linked-recipe-is-a-published-page")
	root := repositoryRootForAIContext(t)
	for _, recipe := range serviceRecipes {
		slug, ok := strings.CutPrefix(recipe.url, "https://putnami.dev/docs/how-to/")
		if !ok {
			t.Fatalf("recipe %s is not a how-to page: %s", recipe.language, recipe.url)
		}
		matches, err := filepath.Glob(filepath.Join(root, "sites", "putnami.dev", "doc", "03-how-to", "*-"+slug+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 {
			t.Fatalf("recipe %s links %s, which %d how-to pages publish; want exactly one", recipe.language, recipe.url, len(matches))
		}
		if !strings.Contains(filepath.Base(matches[0]), "-"+slug+".md") || strings.TrimLeft(strings.SplitN(filepath.Base(matches[0]), "-", 2)[0], "0123456789") != "" {
			t.Fatalf("page %s does not publish at %s", matches[0], recipe.url)
		}
	}
}
