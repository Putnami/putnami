package workspace

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
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

// DetectMarkers is the walk `putnami init` detects languages with: the root
// counts, and the scan's exclusions hold. A marker under a gitignored
// directory, under an excluded one or under .git is never reported.
func TestDetectMarkersReadsNamesAndKeepsTheScanExclusions(t *testing.T) {
	spectest.Proves(t, "cli/init-a-language", "detection-reads-names-only", "detection-skips-excluded-and-ignored-directories")
	root := initScanRepo(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	writeFile(t, filepath.Join(root, ".gitignore"), "generated/\n")
	writeFile(t, filepath.Join(root, "generated", "go.mod"), "module generated\n")
	writeFile(t, filepath.Join(root, "node_modules", "dep", "Cargo.toml"), "")
	writeFile(t, filepath.Join(root, ".git", "hooks", "pom.xml"), "")
	writeFile(t, filepath.Join(root, "services", "api", "App.csproj"), "")
	writeFile(t, filepath.Join(root, "services", "zz", "Other.csproj"), "")
	// A directory named like a file marker marks nothing.
	writeFile(t, filepath.Join(root, "a", "Fake.gemspec", "README"), "")
	writeFile(t, filepath.Join(root, "apps", "ios", "App.xcodeproj", "project.pbxproj"), "")
	scope := func(extension string, markers ...string) ProviderScope {
		return ProviderScope{Extension: extension, Markers: markers, Inputs: markers}
	}
	scopes := []ProviderScope{
		tsScanScope(),
		scope("@putnami/go", "go.mod"),
		scope("@putnami/rust", "Cargo.toml"),
		scope("@putnami/java", "pom.xml"),
		scope("@putnami/dotnet", "*.csproj"),
		scope("@putnami/ruby", "*.gemspec"),
		scope("@putnami/swift", "Package.swift", "*.xcodeproj/project.pbxproj"),
	}
	got := DetectMarkers(root, scopes)
	want := []string{".", "", "", "", "services/api", "", "apps/ios"}
	if !slices.Equal(got, want) {
		t.Errorf("DetectMarkers = %q, want %q", got, want)
	}
	if got := DetectMarkers(root, nil); len(got) != 0 {
		t.Errorf("DetectMarkers without scopes = %q, want nothing", got)
	}
	if got := DetectMarkers(t.TempDir(), scopes); !slices.Equal(got, make([]string, len(scopes))) {
		t.Errorf("DetectMarkers in an empty directory = %q, want no match", got)
	}
}
