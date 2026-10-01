package clientgen

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/clientgen/extension/internal/workspaceclient"
)

// The TypeScript emitter runs Biome's format and lint writer phases over every
// file it emits, so the resolved Biome configuration, its overrides, its
// `extends` chain and the EditorConfig beside it decide the emitted bytes as
// directly as the provider contract does. clientgen-ts is cacheable, which
// makes each of those files a cache-key entry: without them, changing a
// formatting rule serves bytes formatted by the previous rule as a HIT.
//
// This test is the ratchet. workspaceclient.FormatterInputs performs the same
// resolution the emitter's canonicalizer performs; every file it resolves for
// the real repository must be matched by a declared key glob, and a
// configuration that starts reaching an undeclarable file (a package specifier,
// a path outside the workspace) fails resolution rather than being skipped.
func TestClientgenTsCacheKeyCoversEveryResolvedFormatterInput(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "formatter-inputs-are-cache-keyed", "every-resolved-formatter-input-is-a-declared-cache-key-entry")
	manifest := loadExtensionManifest(t)
	task := manifest.Tasks["clientgen-ts"]
	if !task.Cache.IsEnabled() {
		t.Fatal("clientgen-ts is not cacheable; the formatter inputs below are only load-bearing for a cache key")
	}
	if task.Cache.Key == nil {
		t.Fatal("clientgen-ts declares no cache key, so its key falls back to a whole-project hash that skips dot files")
	}
	projectGlobs := task.Cache.Key.Files
	workspaceGlobs := task.Cache.Key.WorkspaceFiles

	workspaceRoot := repositoryRoot(t)
	for _, projectRel := range biomeConfiguredDirectories(t, workspaceRoot) {
		inputs, err := workspaceclient.FormatterInputs(workspaceRoot, projectRel)
		if err != nil {
			t.Fatalf("resolve formatter inputs for %q: %v", projectRel, err)
		}
		for _, input := range inputs {
			if !coveredByKey(input, projectRel, projectGlobs, workspaceGlobs) {
				t.Errorf("formatter input %q (resolved for project %q) is not covered by the clientgen-ts cache key\n"+
					"  files: %v\n  workspaceFiles: %v", input, projectRel, projectGlobs, workspaceGlobs)
			}
		}
	}
}

// TestGenerationTasksKeyOnTheBuiltContract pins the other half of the same
// hazard. With no declared key the file hash walks the project tree and skips
// every dot directory, which is exactly where a built contract lives, so a
// cacheable generator would never see `.gen/schema/openapi.json` move.
func TestGenerationTasksKeyOnTheBuiltContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "formatter-inputs-are-cache-keyed", "a-cacheable-generator-keys-on-the-built-contract-it-reads")
	manifest := loadExtensionManifest(t)
	wantContractInputs := []string{
		".gen/clientgen/config.json",
		".gen/schema/openapi.json",
		".gen/schema/openapi.json.gz",
		"schema/openapi.json",
	}
	for _, name := range []string{"clientgen-go", "clientgen-ts"} {
		task := manifest.Tasks[name]
		if !task.Cache.IsEnabled() {
			t.Errorf("task %q is not cacheable; generation from an unchanged contract must be reusable", name)
			continue
		}
		if task.Cache.Key == nil {
			t.Errorf("task %q declares no cache key, so its key cannot see the built contract it reads", name)
			continue
		}
		declared := map[string]bool{}
		for _, glob := range task.Cache.Key.Files {
			declared[glob] = true
		}
		for _, want := range wantContractInputs {
			if !declared[want] {
				t.Errorf("task %q cache key does not include %q", name, want)
			}
		}
	}
}

// coveredByKey applies the CLI's own two scopes: `files` globs are relative to
// the project the task runs in, `workspaceFiles` globs to the workspace root.
func coveredByKey(input, projectRel string, projectGlobs, workspaceGlobs []string) bool {
	for _, glob := range workspaceGlobs {
		if matched, err := path.Match(glob, input); err == nil && matched {
			return true
		}
	}
	prefix := ""
	if projectRel != "." {
		prefix = strings.TrimSuffix(projectRel, "/") + "/"
	}
	if !strings.HasPrefix(input, prefix) {
		return false
	}
	relative := strings.TrimPrefix(input, prefix)
	for _, glob := range projectGlobs {
		if matched, err := path.Match(glob, relative); err == nil && matched {
			return true
		}
	}
	return false
}

// biomeConfiguredDirectories returns every directory of the real repository
// that owns a biome.json, plus the workspace root itself. Walking rather than
// listing keeps this test honest when a project gains its own configuration.
func biomeConfiguredDirectories(t *testing.T, workspaceRoot string) []string {
	t.Helper()
	seen := map[string]bool{".": true}
	err := filepath.WalkDir(workspaceRoot, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "biome.json" {
			return nil
		}
		rel, relErr := filepath.Rel(workspaceRoot, filepath.Dir(full))
		if relErr != nil {
			return relErr
		}
		seen[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository for Biome configurations: %v", err)
	}
	directories := make([]string, 0, len(seen))
	for directory := range seen {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories
}

// repositoryRoot resolves the workspace this project lives in, so the test
// reads the real configuration rather than a fixture of it.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "putnami.workspace.json")); err != nil {
		t.Fatalf("resolve repository root from %s: %v", root, err)
	}
	return root
}
