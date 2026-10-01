package dependencydocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveUsesPerProjectNearestInstalledPackage(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "resolved-dependency-documentation", "the-nearest-project-package-supplies-exact-documentation")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"name":"root"}`)
	writeFile(t, filepath.Join(root, "node_modules", "@putnami", "web", "package.json"), `{"name":"@putnami/web","version":"1.0.0"}`)
	writeFile(t, filepath.Join(root, "node_modules", "@putnami", "web", "AI.md"), "# Wrong hoisted docs\n")
	writeFile(t, filepath.Join(root, "apps", "shop", "package.json"), `{"name":"shop","dependencies":{"@putnami/web":"2.0.0"}}`)
	writeFile(t, filepath.Join(root, "bun.lock"), "locked bytes\n")
	writeFile(t, filepath.Join(root, "apps", "shop", "node_modules", "@putnami", "web", "package.json"), `{"name":"@putnami/web","version":"2.0.0"}`)
	writeFile(t, filepath.Join(root, "apps", "shop", "node_modules", "@putnami", "web", "AI.md"), "# Exact nested docs\n")

	beforePackage, err := os.ReadFile(filepath.Join(root, "apps", "shop", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeLock, err := os.ReadFile(filepath.Join(root, "bun.lock"))
	if err != nil {
		t.Fatal(err)
	}
	result := Resolve(context.Background(), root, "@putnami/web", "apps/shop")
	if result.Status != "available" || result.Project != "apps/shop" || result.Package != "@putnami/web" || result.Version != "2.0.0" {
		t.Fatalf("result = %+v", result)
	}
	if result.Source == nil || result.Source.Kind != "installed-package" || result.Source.Path != "apps/shop/node_modules/@putnami/web/AI.md" {
		t.Fatalf("source = %+v", result.Source)
	}
	want := sha256.Sum256([]byte("# Exact nested docs\n"))
	if result.Content != "# Exact nested docs\n" || result.Source.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("content = %q, digest = %q", result.Content, result.Source.SHA256)
	}
	afterPackage, _ := os.ReadFile(filepath.Join(root, "apps", "shop", "package.json"))
	afterLock, _ := os.ReadFile(filepath.Join(root, "bun.lock"))
	if string(afterPackage) != string(beforePackage) || string(afterLock) != string(beforeLock) {
		t.Fatal("documentation resolution mutated package.json or bun.lock")
	}
}

func TestResolveClassifiesWorkspaceSymlinkWithoutTrustingExtensionDocs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "resolved-dependency-documentation", "workspace-package-links-retain-their-source-identity")
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	app := filepath.Join(root, "apps", "api")
	dependency := filepath.Join(root, "packages", "application")
	writeFile(t, filepath.Join(app, "package.json"), `{"name":"api","dependencies":{"@putnami/application":"workspace:*"}}`)
	writeFile(t, filepath.Join(dependency, "package.json"), `{"name":"@putnami/application","version":"3.2.1"}`)
	writeFile(t, filepath.Join(dependency, "AI.md"), "# Workspace docs\n")
	if err := os.MkdirAll(filepath.Join(app, "node_modules", "@putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dependency, filepath.Join(app, "node_modules", "@putnami", "application")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".putnami", "extensions", "typescript", "framework-docs", "application", "AI.md"), "# Wrong carried docs\n")

	result := Resolve(context.Background(), root, "@putnami/application", "api")
	if result.Status != "available" || result.Version != "3.2.1" || result.Source == nil || result.Source.Kind != "workspace-replacement" || result.Source.Path != "packages/application/AI.md" {
		t.Fatalf("result = %+v", result)
	}
}

func TestResolveReportsExplicitUnavailableReasons(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "resolved-dependency-documentation", "missing-dependencies-and-documentation-are-explicit")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "package.json"), `{"name":"app","dependencies":{"@putnami/web":"1.0.0"}}`)
	writeFile(t, filepath.Join(root, ".putnami", "extensions", "typescript", "framework-docs", "web", "AI.md"), "# Wrong unversioned docs\n")
	missing := Resolve(context.Background(), root, "@putnami/web", "app")
	if missing.Status != "unavailable" || missing.Reason != "offline_missing" || missing.Fallback == "" || missing.Content != "" || missing.Source != nil {
		t.Fatalf("missing = %+v", missing)
	}

	writeFile(t, filepath.Join(root, "app", "node_modules", "@putnami", "web", "package.json"), `{"name":"@putnami/web","version":"1.0.0"}`)
	withoutDocs := Resolve(context.Background(), root, "@putnami/web", "app")
	if withoutDocs.Reason != "docs_missing" || withoutDocs.Version != "1.0.0" {
		t.Fatalf("without docs = %+v", withoutDocs)
	}

	invalid := Resolve(context.Background(), root, "../secret", "app")
	if invalid.Reason != "unresolved" || invalid.Project != "" || invalid.Package != "" {
		t.Fatalf("invalid = %+v", invalid)
	}
}

func TestResolveRejectsEscapingDocSymlinkAndAmbiguousProject(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "resolved-dependency-documentation", "unsafe-documentation-paths-are-unavailable")
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(root, name, "package.json"), `{"name":"`+name+`","dependencies":{"pkg":"1.0.0"}}`)
	}
	ambiguous := Resolve(context.Background(), root, "pkg", "")
	if ambiguous.Reason != "unresolved" || !strings.Contains(ambiguous.Fallback, "choose one TypeScript project") {
		t.Fatalf("ambiguous = %+v", ambiguous)
	}

	packageDir := filepath.Join(root, "a", "node_modules", "pkg")
	writeFile(t, filepath.Join(packageDir, "package.json"), `{"name":"pkg","version":"1.0.0"}`)
	writeFile(t, filepath.Join(root, "secret.md"), "secret")
	if err := os.Symlink(filepath.Join(root, "secret.md"), filepath.Join(packageDir, "AI.md")); err != nil {
		t.Fatal(err)
	}
	escaping := Resolve(context.Background(), root, "pkg", "a")
	if escaping.Reason != "docs_missing" || escaping.Content != "" || escaping.Source != nil {
		t.Fatalf("escaping = %+v", escaping)
	}
}

func TestResolveRejectsNonRegularDocumentation(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "app", "node_modules", "pkg")
	writeFile(t, filepath.Join(root, "app", "package.json"), `{"name":"app","dependencies":{"pkg":"1.0.0"}}`)
	writeFile(t, filepath.Join(packageDir, "package.json"), `{"name":"pkg","version":"1.0.0"}`)
	if err := os.Mkdir(filepath.Join(packageDir, "AI.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := Resolve(context.Background(), root, "pkg", "app")
	if result.Reason != "docs_missing" || result.Content != "" || result.Source != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestResolveStopsProjectDiscoveryWhenTheCallerIsDone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "package.json"), `{"name":"app","dependencies":{"@scope/dep":"1.0.0"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := Resolve(ctx, root, "@scope/dep", "")
	if result.Status != StatusUnavailable || result.Reason != ReasonUnresolved {
		t.Fatalf("canceled resolve = %+v, want an explicit unresolved result", result)
	}
	if !strings.Contains(result.Fallback, "project discovery is unavailable") {
		t.Fatalf("canceled resolve fallback = %q", result.Fallback)
	}
}
