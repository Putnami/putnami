package clientgen

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
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

// The TypeScript emitter reads the workspace root package.json to decide how a
// client depends on @putnami/client (`catalog:`, a pinned version, or
// `workspace:*`). That choice is an emitted byte, so the file keys clientgen-ts.
func TestClientgenTsCacheKeyCoversTheWorkspaceRootPackageJSON(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "formatter-inputs-are-cache-keyed", "the-workspace-root-package-json-keys-the-typescript-target")
	task := loadExtensionManifest(t).Tasks["clientgen-ts"]
	if task.Cache.Key == nil {
		t.Fatal("clientgen-ts declares no cache key")
	}
	if !coveredByKey("package.json", "services/provider", task.Cache.Key.Files, task.Cache.Key.WorkspaceFiles) {
		t.Fatalf("the workspace root package.json is not a clientgen-ts cache key entry\n  workspaceFiles: %v", task.Cache.Key.WorkspaceFiles)
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

// biomeConfiguredDirectories returns the root and every directory with a
// Biome configuration in Git's tracked or non-ignored untracked candidate set.
// That is the same source surface the CLI uses for Git-backed cache inputs.
func biomeConfiguredDirectories(t *testing.T, workspaceRoot string) []string {
	t.Helper()
	seen := map[string]bool{".": true}
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = workspaceRoot
	paths, err := cmd.Output()
	if err != nil {
		t.Fatalf("list Git candidate files for Biome configurations: %v", err)
	}
	if len(paths) > 0 && paths[len(paths)-1] != 0 {
		t.Fatal("Git candidate paths are not NUL-terminated")
	}
	for _, candidate := range bytes.Split(paths, []byte{0}) {
		if len(candidate) == 0 {
			continue
		}
		rel := filepath.FromSlash(string(candidate))
		if filepath.Base(rel) != "biome.json" {
			continue
		}
		info, statErr := os.Stat(filepath.Join(workspaceRoot, rel))
		if os.IsNotExist(statErr) {
			continue // Git also lists tracked files deleted from the worktree.
		}
		if statErr != nil {
			t.Fatalf("inspect Biome configuration %q: %v", rel, statErr)
		}
		if info.Mode().IsRegular() {
			seen[filepath.ToSlash(filepath.Dir(rel))] = true
		}
	}
	directories := make([]string, 0, len(seen))
	for directory := range seen {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories
}

func TestBiomeConfiguredDirectoriesFollowGitCandidates(t *testing.T) {
	root := t.TempDir()
	write := func(rel, contents string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init", "--quiet")
	write(".gitignore", "ignored/\n")
	write("tracked/biome.json", "{}")
	write("untracked/biome.json", "{}")
	write("ignored/nested/biome.json", "{}")
	runGit("add", ".gitignore", "tracked/biome.json")

	if got, want := biomeConfiguredDirectories(t, root), []string{".", "tracked", "untracked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Biome configuration directories = %v, want %v", got, want)
	}
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
