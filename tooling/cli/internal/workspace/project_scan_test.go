package workspace

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/sdk/extension/gittest"
)

// tsScanScope is the TypeScript adapter's declaration. A package.json marks a
// project only because an adapter says so — core's own marker table is gone —
// so a scan test about package.json directories has to supply the provider
// that claims them.
func tsScanScope() ProviderScope {
	return ProviderScope{
		Extension: "@putnami/typescript",
		Markers:   []string{"package.json"},
		Inputs:    []string{"package.json"},
		Excludes:  []string{"node_modules"},
	}
}

func initScanRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-b", "main"},
	}
	for _, args := range cmds {
		cmd := gittest.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestInitScanRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string { return initScanRepo(t) })
}

func TestScanProjectPaths_SkipsGitIgnoredDirs(t *testing.T) {
	root := initScanRepo(t)
	// Real source project — tracked, must be discovered.
	writeFile(t, filepath.Join(root, "sample", "package.json"), "{}")
	// Generated, gitignored client — must NOT be discovered.
	writeFile(t, filepath.Join(root, ".gitignore"), "**/clients/ts/\n")
	writeFile(t, filepath.Join(root, "sample", "clients", "ts", "package.json"), "{}")

	paths, err := ScanProjectPathsWithProviders(root, []ProviderScope{tsScanScope()})
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Contains(paths, "sample") {
		t.Errorf("tracked project %q should be discovered; got %v", "sample", paths)
	}
	if slices.Contains(paths, "sample/clients/ts") {
		t.Errorf("gitignored generated dir %q should be skipped; got %v", "sample/clients/ts", paths)
	}
}

func TestScanProjectPaths_NonGitRepoUnaffected(t *testing.T) {
	root := t.TempDir() // not a git repo: nothing extra is skipped
	writeFile(t, filepath.Join(root, "app", "package.json"), "{}")

	paths, err := ScanProjectPathsWithProviders(root, []ProviderScope{tsScanScope()})
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Contains(paths, "app") {
		t.Errorf("expected %q in %v", "app", paths)
	}
}

// A scope-only putnami.json marks no project, even for an adapter that declares
// putnami.json as a marker. The directory's other manifests still count.
func TestScanProjectPaths_ScopeOnlyConfigIgnoresPutnamiJSONMarker(t *testing.T) {
	root := t.TempDir()
	scope := `{"$schema": "https://putnami.dev/schemas/putnami-scope.json", "includes": ["app"], "tags": ["go"]}`
	writeFile(t, filepath.Join(root, "scope", "putnami.json"), scope)
	writeFile(t, filepath.Join(root, "scope", "app", "putnami.json"), `{"name": "app"}`)
	writeFile(t, filepath.Join(root, "manifest-scope", "putnami.json"), scope)
	writeFile(t, filepath.Join(root, "manifest-scope", "package.json"), "{}")

	cloud := ProviderScope{Extension: "@putnami/cloud", Markers: []string{"putnami.json"}}
	paths, err := ScanProjectPathsWithProviders(root, []ProviderScope{cloud, tsScanScope()})
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if want := []string{"manifest-scope", "scope/app"}; !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}
