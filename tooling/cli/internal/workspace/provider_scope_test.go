package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
)

func TestNewProviderScope_NormalizesAndSorts(t *testing.T) {
	scope, ok := NewProviderScope("  @putnami/typescript  ", &extproto.WorkspaceAdapter{
		Markers:  []string{"./package.json", "package.json"},
		Inputs:   []string{"tsconfig.json", "package.json", "bun.lock", "package.json"},
		Excludes: []string{"node_modules", "dist"},
		SyncTask: " workspace-sync-exec ",
	})
	if !ok {
		t.Fatal("valid adapter rejected")
	}
	if scope.Extension != "@putnami/typescript" || scope.SyncTask != "workspace-sync-exec" {
		t.Fatalf("scope = %+v", scope)
	}
	// A pattern SET has no order, so the normalized form must not depend on the
	// order the manifest happened to author it in: the scan, the recorded
	// inputs and therefore the snapshot digest all read this.
	if !slices.Equal(scope.Markers, []string{"package.json"}) {
		t.Errorf("markers = %v, want deduped", scope.Markers)
	}
	if !slices.Equal(scope.Inputs, []string{"bun.lock", "package.json", "tsconfig.json"}) {
		t.Errorf("inputs = %v, want sorted and deduped", scope.Inputs)
	}
}

func TestNewProviderScope_RejectsUnusableAdapters(t *testing.T) {
	cases := map[string]*extproto.WorkspaceAdapter{
		"no markers": {Inputs: []string{"package.json"}},
		"no inputs":  {Markers: []string{"package.json"}},
		"nil":        nil,
	}
	for name, adapter := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := NewProviderScope("x", adapter); ok {
				t.Fatal("an adapter with no marker or no input must contribute nothing")
			}
		})
	}
	if _, ok := NewProviderScope("", &extproto.WorkspaceAdapter{
		Markers: []string{"package.json"}, Inputs: []string{"package.json"},
	}); ok {
		t.Fatal("an unnamed provider cannot key a metadata bucket")
	}
}

// A declaration is candidate-directory-relative, so it must claim the same
// filename at any depth. This is the rule that decides which provider owns a
// changed input, and therefore which single provider gets re-probed.
func TestProviderScope_MatchesInputAtAnyDepth(t *testing.T) {
	scope, _ := NewProviderScope("ts", &extproto.WorkspaceAdapter{
		Markers: []string{"package.json"},
		Inputs:  []string{"package.json", "config/tsconfig.json", "**/*.go"},
	})
	matches := []string{
		"package.json", "web/package.json", "a/b/c/package.json", "web/config/tsconfig.json",
		"app/main.go", "app/cmd/tool/main.go", "app/**/*.go",
	}
	for _, rel := range matches {
		if !scope.MatchesInput(rel) {
			t.Errorf("MatchesInput(%q) = false, want true", rel)
		}
	}
	misses := []string{"go.mod", "web/package.json.bak", "packagexjson", "tsconfig.json", "web/config/other.json"}
	for _, rel := range misses {
		if scope.MatchesInput(rel) {
			t.Errorf("MatchesInput(%q) = true, want false", rel)
		}
	}
}

// A marker is always an input (the protocol enforces it), so creating or
// removing one is already an input change the owning provider claims — which is
// what makes "creating a declared marker triggers discovery for that provider"
// true without a second predicate.
func TestProviderScope_MarkerChangesAreInputChanges(t *testing.T) {
	scope, _ := NewProviderScope("dotnet", &extproto.WorkspaceAdapter{
		Markers: []string{"*.csproj"},
		Inputs:  []string{"*.csproj"},
	})
	for _, marker := range scope.Markers {
		if !slices.Contains(scope.Inputs, marker) {
			t.Fatalf("marker %q is not an input; a marker nobody hashes leaves the snapshot valid "+
				"and the answer stale", marker)
		}
	}
	if !scope.MatchesInput("apps/api/Api.csproj") {
		t.Error("a glob marker must be claimed at depth")
	}
	if scope.MatchesInput("apps/api/Api.sln") {
		t.Error("an unrelated file must not be claimed")
	}
}

// The scan is widened by extension-declared markers, and the core-owned
// exclusions win over every adapter: `.git`, `.putnami` and gitignored
// directories are never descended into. Generated output routinely contains a
// package.json, and discovering one turns a build artifact into a build input.
func TestScanProjectPathsWithProviders_HonorsCoreAndAdapterExclusions(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"apps/api/Api.csproj",
		"apps/api/obj/Debug/Generated.csproj",
		".git/hooks/Hook.csproj",
		".putnami/out/Out.csproj",
		"generated/client/Gen.csproj",
	} {
		writeFileAt(t, filepath.Join(root, rel), "<Project/>")
	}
	writeFileAt(t, filepath.Join(root, ".gitignore"), "generated/\n")
	initGitRepo(t, root)

	scopes := []ProviderScope{mustScope(t, "dotnet", &extproto.WorkspaceAdapter{
		Markers:  []string{"*.csproj"},
		Inputs:   []string{"*.csproj"},
		Excludes: []string{"obj"},
	})}

	found, err := ScanProjectPathsWithProviders(root, scopes)
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Equal(found, []string{"apps/api"}) {
		t.Fatalf("candidates = %v, want only [apps/api]: the adapter's own exclude, `.git`, `.putnami` "+
			"and the gitignored directory must all be refused", found)
	}
}

// Adapter markers are the ONLY way a language manifest marks a project since
// core's own table was deleted. An out-of-tree language core
// has never heard of is discoverable on exactly the same terms as Go — which is
// the property the deletion was for.
func TestScanProjectPathsWithProviders_MarkersAreTheOnlyLanguageTable(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n")
	writeFileAt(t, filepath.Join(root, "apps", "api", "Api.csproj"), "<Project/>")

	scopes := []ProviderScope{
		mustScope(t, "dotnet", &extproto.WorkspaceAdapter{
			Markers: []string{"*.csproj"}, Inputs: []string{"*.csproj"},
		}),
		mustScope(t, "@putnami/go", &extproto.WorkspaceAdapter{
			Markers: []string{"go.mod"}, Inputs: []string{"go.mod"},
		}),
	}
	found, err := ScanProjectPathsWithProviders(root, scopes)
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Equal(found, []string{"apps/api", "svc"}) {
		t.Fatalf("candidates = %v, want both adapter-marked directories", found)
	}

	// Without the adapters, NEITHER directory is visible — core knows only its
	// own putnami.json, and a go.mod is no more privileged than a .csproj.
	bare, err := ScanProjectPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bare) != 0 {
		t.Fatalf("bare scan = %v, want none: core has no language marker table left", bare)
	}
}

// A glob metadata input records the whole matched SET, so creating, deleting or
// editing a matching file all invalidate. A per-file record cannot express the
// creation case, because a file that does not exist yet has no path to record.
func TestGlobInput_SetChangesInvalidate(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "web", "tsconfig.json"), `{"compilerOptions":{}}`)

	baseline := globInput(root, "web/tsconfig*.json")
	if baseline.Kind != inputKindGlob {
		t.Fatalf("kind = %q, want %q", baseline.Kind, inputKindGlob)
	}
	if inputStillMatches(root, baseline) != true {
		t.Fatal("an unchanged set must still match")
	}

	// Creation.
	writeFileAt(t, filepath.Join(root, "web", "tsconfig.build.json"), `{}`)
	if inputStillMatches(root, baseline) {
		t.Error("adding a matching file did not invalidate the glob input")
	}

	// Modification.
	afterCreate := globInput(root, "web/tsconfig*.json")
	writeFileAt(t, filepath.Join(root, "web", "tsconfig.build.json"), `{"x":1}`)
	if inputStillMatches(root, afterCreate) {
		t.Error("editing a matching file did not invalidate the glob input")
	}

	// Deletion.
	afterEdit := globInput(root, "web/tsconfig*.json")
	if err := os.Remove(filepath.Join(root, "web", "tsconfig.build.json")); err != nil {
		t.Fatal(err)
	}
	if inputStillMatches(root, afterEdit) {
		t.Error("removing a matching file did not invalidate the glob input")
	}
}

// Recursive workspace inputs are classification witnesses: unlike a task
// input, the recorded glob has to notice a matching file that did not exist
// when the snapshot was written. `**` therefore means zero or more directory
// segments here too, not filepath.Glob's single-segment `*` behavior.
func TestGlobInput_RecursiveSetChangesInvalidateAtAnyDepth(t *testing.T) {
	for _, created := range []string{
		"main.go",
		filepath.Join("cmd", "tool", "main.go"),
	} {
		t.Run(filepath.ToSlash(created), func(t *testing.T) {
			root := t.TempDir()
			writeFileAt(t, filepath.Join(root, "app", "lib.go"), "package lib\n")

			baseline := globInput(root, "app/**/*.go")
			if baseline.Kind != inputKindGlob {
				t.Fatalf("kind = %q, want %q", baseline.Kind, inputKindGlob)
			}
			if !inputStillMatches(root, baseline) {
				t.Fatal("an unchanged recursive input must still match")
			}

			writeFileAt(t, filepath.Join(root, "app", created), "package main\n\nfunc main() {}\n")
			if inputStillMatches(root, baseline) {
				t.Errorf("creating %s did not invalidate app/**/*.go", filepath.ToSlash(created))
			}
		})
	}
}

func TestProviderInputs_AreBoundedAndDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "web", "package.json"), `{"name":"web"}`)

	first := providerInputs(root, []string{"web", "svc"}, []string{"package.json", "bun.lock"})
	if len(first) != 4 {
		t.Fatalf("inputs = %d, want one per (directory, pattern) pair: %+v", len(first), first)
	}
	// A missing input is recorded as ABSENT rather than skipped, so its later
	// creation invalidates the snapshot.
	var absent int
	for _, input := range first {
		if input.Digest == absentInputDigest {
			absent++
		}
	}
	if absent != 3 {
		t.Errorf("absent inputs = %d, want 3 (only web/package.json exists)", absent)
	}

	second := providerInputs(root, []string{"svc", "web"}, []string{"bun.lock", "package.json"})
	for i := range first {
		if first[i].Path != second[i].Path || first[i].Digest != second[i].Digest {
			t.Fatalf("input %d differs across argument order: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func mustScope(t *testing.T, name string, adapter *extproto.WorkspaceAdapter) ProviderScope {
	t.Helper()
	scope, ok := NewProviderScope(name, adapter)
	if !ok {
		t.Fatalf("NewProviderScope(%q) rejected the adapter", name)
	}
	return scope
}

func initGitRepo(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; gitignore exclusion is untestable here")
	}
	for _, args := range [][]string{{"init"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v: %s", args, err, out)
		}
	}
}

// A recursive metadata input is a witness on the bytes a provider's answer is
// derived from, and no provider derives an answer from an install tree or a
// generated one. The adapter contract already says core excludes gitignored
// directories for every extension; the recorded set applies the same rule, so
// `**/*.ts` does not walk `node_modules` and a cold clone agrees with a warm
// checkout of the same commit.
func TestGlobInput_SkipsGitIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v: %s", err, out)
		}
	}
	writeFileAt(t, filepath.Join(root, ".gitignore"), "node_modules/\n")
	writeFileAt(t, filepath.Join(root, "web", "src", "index.ts"), "export const a = 1\n")
	writeFileAt(t, filepath.Join(root, "web", "node_modules", "dep", "index.ts"), "export const b = 1\n")

	baseline := globInput(root, "web/**/*.ts")
	if !inputStillMatches(root, baseline) {
		t.Fatal("an unchanged set must still match")
	}

	// An installed package is not this package's source: editing it changes no
	// probe answer and must not invalidate the snapshot.
	writeFileAt(t, filepath.Join(root, "web", "node_modules", "dep", "index.ts"), "export const b = 2\n")
	if !inputStillMatches(root, baseline) {
		t.Error("editing an ignored directory invalidated the recursive input")
	}

	// The package's own source still witnesses.
	writeFileAt(t, filepath.Join(root, "web", "src", "index.ts"), "export const a = 2\n")
	if inputStillMatches(root, baseline) {
		t.Error("editing a tracked source file did not invalidate the recursive input")
	}
}

// Edge attribution is negotiated: core asks for it only from an adapter that
// declares it, because a provider built before the request member rejects a
// request carrying it. The scope is where that declaration reaches the probe.
func TestNewProviderScope_CarriesTheAttributionCapability(t *testing.T) {
	for _, declared := range []bool{false, true} {
		scope, ok := NewProviderScope("@putnami/go", &extproto.WorkspaceAdapter{
			Markers:           []string{"go.mod"},
			Inputs:            []string{"go.mod"},
			DependencySources: declared,
		})
		if !ok {
			t.Fatal("NewProviderScope rejected a valid adapter")
		}
		if scope.DependencySources != declared {
			t.Errorf("declared=%t: scope.DependencySources = %t", declared, scope.DependencySources)
		}
	}
}
