package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestLintCommandAndTasksCoverEveryBiomeWritableProjectFile keeps command
// activation and task cache inputs aligned with Biome. This prevents a
// status-only lint cache entry from describing a clean tree when Biome rewrote
// a test, config, or other project file outside src/, and avoids
// invoking Biome in a project with no supported files. The read-only lint task
// excludes generated output from its cache key: .gen is ignored by lint and
// changes whenever generation runs, so hashing it makes unchanged source miss.
func TestLintCommandAndTasksCoverEveryBiomeWritableProjectFile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "lint-covers-every-biome-writable-project-file")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}

	var manifest struct {
		Commands map[string]struct {
			ActivationFiles []string `json:"activationFiles"`
		} `json:"commands"`
		Tasks map[string]struct {
			Inputs map[string]struct {
				Files []string `json:"files"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse extension manifest: %v", err)
	}

	biome := []string{
		"**/*.ts", "**/*.tsx", "**/*.js", "**/*.jsx", "**/*.mts", "**/*.cts", "**/*.mjs", "**/*.cjs",
		"**/*.json", "**/*.jsonc", "**/*.css", "**/*.graphql", "**/*.gql", "**/*.html", "**/*.htm", "**/*.grit",
	}
	// ESLint plugins also lint these; an ESLint project's lint must activate
	// on its components and re-key on every file a plugin may read.
	components := []string{"**/*.vue", "**/*.svelte", "**/*.astro"}
	documents := []string{"**/*.md", "**/*.mdx", "**/*.yml", "**/*.yaml"}
	activation := append(append([]string(nil), biome...), components...)
	want := append(append([]string(nil), activation...), documents...)
	if got := manifest.Commands["lint"].ActivationFiles; !reflect.DeepEqual(got, activation) {
		t.Errorf("lint activation files = %v, want %v", got, activation)
	}
	for _, taskName := range []string{"lint-format", "lint-check"} {
		task, ok := manifest.Tasks[taskName]
		if !ok {
			t.Fatalf("manifest task %q is missing", taskName)
		}
		if got := task.Inputs["sources"].Files; !reflect.DeepEqual(got, want) {
			t.Errorf("%s sources cache inputs = %v, want %v", taskName, got, want)
		}
	}

	lintAll, ok := manifest.Tasks["lint-all"]
	if !ok {
		t.Fatal("manifest task \"lint-all\" is missing")
	}
	wantLintAll := append(append([]string(nil), want...), "!.gen/**")
	if got := lintAll.Inputs["sources"].Files; !reflect.DeepEqual(got, wantLintAll) {
		t.Errorf("lint-all sources cache inputs = %v, want %v", got, wantLintAll)
	}
}

// TestLintTasksKeyESLintConfigurationAndIgnoreFiles keeps a cached lint result
// from describing a project whose ESLint configuration or ignore file changed,
// in the project or at the workspace root. The file types ESLint plugins lint
// are keyed by TestLintCommandAndTasksCoverEveryBiomeWritableProjectFile.
func TestLintTasksKeyESLintConfigurationAndIgnoreFiles(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-runs-eslint", "eslint-configuration-ignore-and-plugin-files-are-cache-inputs")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				From  string   `json:"from"`
				Files []string `json:"files"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse extension manifest: %v", err)
	}
	want := []string{
		"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", "eslint.config.mts", "eslint.config.cts",
		".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yaml", ".eslintrc.yml", ".eslintignore",
	}
	for _, taskName := range []string{"lint-all", "lint-format", "lint-check"} {
		task, ok := manifest.Tasks[taskName]
		if !ok {
			t.Fatalf("manifest task %q is missing", taskName)
		}
		for _, input := range []struct{ name, from string }{{"config", "project"}, {"workspaceConfig", "workspace"}} {
			binding := task.Inputs[input.name]
			if binding.From != input.from {
				t.Errorf("%s %s from = %q, want %q", taskName, input.name, binding.From, input.from)
			}
			for _, file := range want {
				if !slices.Contains(binding.Files, file) {
					t.Errorf("%s %s files %v miss %s", taskName, input.name, binding.Files, file)
				}
			}
		}
	}
}
