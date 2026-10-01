package dependencydocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
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

func TestResolveReturnsExactWorkspaceReplacementBytes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "resolved-dependency-documentation", "exact-selected-module-documentation-is-returned")
	root := t.TempDir()
	app := filepath.Join(root, "apps", "api")
	dep := filepath.Join(root, "go", "app")
	writeFile(t, filepath.Join(app, "go.mod"), "module example.test/api\n\ngo 1.25\n\nrequire go.putnami.dev/app v1.4.2\n")
	writeFile(t, filepath.Join(dep, "AI.md"), "# Exact app docs\n")

	result := resolve(context.Background(), root, "", "go.putnami.dev/app", "apps/api",
		func(_ context.Context, query moduleQuery) (listedModule, error) {
			realApp, err := filepath.EvalSymlinks(app)
			if err != nil {
				t.Fatal(err)
			}
			if query.ProjectDir != realApp || query.Reference != "go.putnami.dev/app" {
				t.Fatalf("list request = %+v", query)
			}
			return listedModule{Path: query.Reference, Main: true, Dir: dep}, nil
		})

	if result.Status != StatusAvailable || result.Project != "apps/api" || result.Package != "go.putnami.dev/app" || result.Version != "v1.4.2" {
		t.Fatalf("result = %+v, source = %+v", result, result.Source)
	}
	if result.Source == nil || result.Source.Kind != "workspace-replacement" || result.Source.Path != "go/app/AI.md" {
		t.Fatalf("source = %+v", result.Source)
	}
	wantDigest := sha256.Sum256([]byte("# Exact app docs\n"))
	if result.Source.SHA256 != hex.EncodeToString(wantDigest[:]) || result.Content != "# Exact app docs\n" {
		t.Fatalf("returned bytes = %#v, digest = %q", result.Content, result.Source.SHA256)
	}
}

func TestResolveDoesNotSubstituteExtensionCarriedDocsForMissingModule(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "resolved-dependency-documentation", "extension-carried-documentation-is-never-mislabeled")
	root := t.TempDir()
	app := filepath.Join(root, "api")
	extension := filepath.Join(root, ".putnami", "extensions", "go")
	writeFile(t, filepath.Join(app, "go.mod"), "module example.test/api\n\nrequire go.putnami.dev/app v1.4.2\n")
	writeFile(t, filepath.Join(extension, "framework-docs", "app", "AI.md"), "# Wrong unversioned copy\n")

	result := resolve(context.Background(), root, extension, "go.putnami.dev/app", "api",
		func(context.Context, moduleQuery) (listedModule, error) {
			return listedModule{}, os.ErrNotExist
		})
	if result.Status != StatusUnavailable || result.Reason != ReasonOfflineMissing || result.Content != "" || result.Source != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestResolveRejectsDocumentationSymlinkOutsideModule(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "resolved-dependency-documentation", "unsafe-documentation-paths-are-unavailable")
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	app := filepath.Join(root, "api")
	moduleDir := filepath.Join(root, "cache", "module")
	writeFile(t, filepath.Join(app, "go.mod"), "module example.test/api\n\nrequire example.test/module v1.0.0\n")
	writeFile(t, filepath.Join(root, "secret.md"), "secret")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "secret.md"), filepath.Join(moduleDir, "AI.md")); err != nil {
		t.Fatal(err)
	}

	result := resolve(context.Background(), root, "", "example.test/module", "api",
		func(context.Context, moduleQuery) (listedModule, error) {
			return listedModule{Path: "example.test/module", Version: "v1.0.0", Dir: moduleDir}, nil
		})
	if result.Status != StatusUnavailable || result.Reason != ReasonDocsMissing || result.Content != "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestResolveRejectsNonRegularDocumentation(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "api")
	moduleDir := filepath.Join(root, "cache", "module")
	writeFile(t, filepath.Join(app, "go.mod"), "module example.test/api\n\nrequire example.test/module v1.0.0\n")
	if err := os.MkdirAll(filepath.Join(moduleDir, "AI.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := resolve(context.Background(), root, "", "example.test/module", "api",
		func(context.Context, moduleQuery) (listedModule, error) {
			return listedModule{Path: "example.test/module", Version: "v1.0.0", Dir: moduleDir}, nil
		})
	if result.Reason != ReasonDocsMissing || result.Content != "" || result.Source != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestResolveWithCmdGoIsOfflineAndDoesNotMutateModuleFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "resolved-dependency-documentation", "documentation-resolution-is-offline-and-read-only")
	if _, err := resolveGoBinary(moduleQuery{}); err != nil {
		t.Skipf("Go toolchain unavailable: %v", err)
	}
	root := t.TempDir()
	app := filepath.Join(root, "app")
	dep := filepath.Join(root, "dep")
	files := map[string]string{
		"go.work":     "go 1.25.0\n\nuse (\n\t./app\n\t./dep\n)\n",
		"app/go.mod":  "module example.test/app\n\ngo 1.25.0\n\nrequire example.test/dep v1.2.3\n",
		"app/go.sum":  "",
		"dep/go.mod":  "module example.test/dep\n\ngo 1.25.0\n",
		"dep/AI.md":   "# Dependency docs\n",
		"go.work.sum": "",
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(root, rel), content)
	}
	before := snapshotFiles(t, root, []string{"go.work", "go.work.sum", "app/go.mod", "app/go.sum", "dep/go.mod"})
	t.Setenv("GOWORK", filepath.Join(root, "go.work"))

	result := Resolve(context.Background(), root, "", "example.test/dep", "app")
	if result.Status != StatusAvailable || result.Version != "v1.2.3" || result.Source == nil || result.Source.Kind != "workspace-replacement" {
		t.Fatalf("result = %+v, source = %+v", result, result.Source)
	}
	after := snapshotFiles(t, root, []string{"go.work", "go.work.sum", "app/go.mod", "app/go.sum", "dep/go.mod"})
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("module files changed:\nbefore: %#v\nafter:  %#v", before, after)
	}
	_ = app
	_ = dep
}

func TestOfflineGoEnvPreservesHumanWorkspaceSelection(t *testing.T) {
	for _, selected := range []string{"off", filepath.Join(t.TempDir(), "custom.work")} {
		env := offlineGoEnv([]string{"PATH=/bin", "GOWORK=" + selected}, t.TempDir())
		if got := envValue(env, "GOWORK"); got != selected {
			t.Errorf("GOWORK = %q, want inherited %q", got, selected)
		}
		if got := envValue(env, "GOPROXY"); got != "off" {
			t.Errorf("GOPROXY = %q, want offline", got)
		}
	}
}

func snapshotFiles(t *testing.T, root string, rels []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(rels))
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		result[rel] = string(data)
	}
	return result
}

func TestResolveRejectsAmbiguousProjectAndInvalidReference(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(root, name, "go.mod"), "module example.test/"+name+"\n\nrequire example.test/dep v1.0.0\n")
	}
	list := func(context.Context, moduleQuery) (listedModule, error) {
		t.Fatal("module resolver must not run")
		return listedModule{}, nil
	}
	ambiguous := resolve(context.Background(), root, "", "example.test/dep", "", list)
	if ambiguous.Reason != ReasonUnresolved || !strings.Contains(ambiguous.Fallback, "choose one Go project") {
		t.Fatalf("ambiguous result = %+v", ambiguous)
	}
	invalid := resolve(context.Background(), root, "", "../secret", "a", list)
	if invalid.Reason != ReasonUnresolved || invalid.Project != "" {
		t.Fatalf("invalid result = %+v", invalid)
	}
}
