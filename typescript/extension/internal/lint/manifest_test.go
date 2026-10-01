package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

	want := []string{
		"**/*.ts", "**/*.tsx", "**/*.js", "**/*.jsx", "**/*.mts", "**/*.cts", "**/*.mjs", "**/*.cjs",
		"**/*.json", "**/*.jsonc", "**/*.css", "**/*.graphql", "**/*.gql", "**/*.html", "**/*.htm", "**/*.grit",
	}
	if got := manifest.Commands["lint"].ActivationFiles; !reflect.DeepEqual(got, want) {
		t.Errorf("lint activation files = %v, want %v", got, want)
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
