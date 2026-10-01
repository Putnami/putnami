package pkg

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// ---- loadIntoDaemon ----
//
// loadIntoDaemon stages the assembled image as a docker-load tarball and shells
// out to `docker load`. These tests drive its tarball-staging branch and error
// wrapping without ever invoking docker: an unreadable/empty OCI layout makes
// the tarball write fail first, so the docker daemon is never contacted.

func TestLoadIntoDaemon_MissingLayoutFailsBeforeDocker(t *testing.T) {
	// A non-existent layout dir makes WriteDockerTarball fail in LoadFromLayout,
	// so loadIntoDaemon returns its "writing docker tarball" wrap and never runs
	// `docker load`.
	missing := filepath.Join(t.TempDir(), "no-such-layout")

	err := loadIntoDaemon(missing, "example.com/app:c-deadbeef", nil)
	if err == nil {
		t.Fatal("expected an error for a missing OCI layout")
	}
	if !strings.Contains(err.Error(), "writing docker tarball") {
		t.Errorf("error = %q, want it to wrap the tarball write failure", err.Error())
	}
}

func TestLoadIntoDaemon_EmptyLayoutFailsBeforeDocker(t *testing.T) {
	// An existing but empty layout dir (no index.json) also fails the tarball
	// write before any docker invocation.
	emptyLayout := t.TempDir()

	err := loadIntoDaemon(emptyLayout, "example.com/app:c-deadbeef", []string{"example.com/app:latest"})
	if err == nil {
		t.Fatal("expected an error for an empty OCI layout")
	}
	if !strings.Contains(err.Error(), "writing docker tarball") {
		t.Errorf("error = %q, want it to wrap the tarball write failure", err.Error())
	}
}

// ---- contentTagFor ----

func TestContentTagFor(t *testing.T) {
	if got := contentTagFor("abc123def456"); got != "c-abc123def456" {
		t.Errorf("contentTagFor() = %q, want %q", got, "c-abc123def456")
	}
}

func TestContentTagFor_Empty(t *testing.T) {
	if got := contentTagFor(""); got != "c-" {
		t.Errorf("contentTagFor(\"\") = %q, want %q", got, "c-")
	}
}

// ---- assetFiles ----

func TestAssetFiles_MapsTreeUnderAppGen(t *testing.T) {
	genDir := t.TempDir()
	// Nested asset to confirm relative paths are preserved under /app/.gen.
	publicDir := filepath.Join(genDir, "public", "css")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicDir, "style.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := assetFiles(genDir)
	if err != nil {
		t.Fatalf("assetFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}

	paths := []string{files[0].Path, files[1].Path}
	sort.Strings(paths)
	want := []string{"/app/.gen/manifest.json", "/app/.gen/public/css/style.css"}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("path[%d] = %q, want %q", i, paths[i], w)
		}
	}
	for _, f := range files {
		if f.Mode != 0o644 {
			t.Errorf("asset %q mode = %o, want 0644", f.Path, f.Mode)
		}
		if f.Source == "" {
			t.Errorf("asset %q has empty source", f.Path)
		}
	}
}

func TestAssetFiles_EmptyDir(t *testing.T) {
	files, err := assetFiles(t.TempDir())
	if err != nil {
		t.Fatalf("assetFiles on empty dir: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no files for empty dir, got %d", len(files))
	}
}

// ---- public-asset expectation (fail-loud guard) ----

func TestProjectExpectsPublicAssets(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "declared-assets-must-exist", "a-project-declaring-public-assets-is-recognized")
	withConfig := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"asset under public/", `{"options":{"generate":{"assets":[{"from":"/doc","to":"public/docs"}]}}}`, true},
		{"asset is public exactly", `{"options":{"generate":{"assets":[{"from":"/x","to":"public"}]}}}`, true},
		{"non-public asset", `{"options":{"generate":{"assets":[{"from":"/x","to":"config/biome.json"}]}}}`, false},
		{"no generate assets", `{"options":{}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectExpectsPublicAssets(withConfig(t, tc.body)); got != tc.want {
				t.Errorf("projectExpectsPublicAssets = %v, want %v", got, tc.want)
			}
		})
	}

	if projectExpectsPublicAssets(t.TempDir()) {
		t.Error("a project with no config should not expect public assets")
	}
}

// When a project declares generate assets under public/ but .gen/public is
// missing at package time, PackageDocker must fail rather than silently ship an
// asset-less image (the docs-less deploy that 404s). The error fires at the
// asset check, before any OCI assembly, so no registry or base image is touched.
func TestPackageDocker_FailsLoudWhenPublicAssetsDeclaredButMissing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "declared-assets-must-exist", "declared-but-absent-public-assets-fail-packaging")
	ws := t.TempDir()
	projPath := filepath.Join("sites", "site")
	projDir := filepath.Join(ws, projPath)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "putnami.json"),
		[]byte(`{"options":{"generate":{"assets":[{"from":"/doc","to":"public/docs"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stage a compile output with a platform binary so packaging reaches the
	// asset check (it errors earlier without one).
	compileOut := filepath.Join(ws, ".putnami", "out", projPath, "build", "compile")
	if err := os.MkdirAll(compileOut, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compileOut, "app-linux-x64"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// No .gen/public — the generate output was lost.
	_, err := PackageDocker(ws, "@putnami/site", projPath, DockerParams{Platform: "linux/amd64"}, nil, "0.1.0")
	if err == nil {
		t.Fatal("expected an error when public assets are declared but .gen/public is missing")
	}
	if !strings.Contains(err.Error(), ".gen") || !strings.Contains(err.Error(), "public") {
		t.Errorf("error = %v, want it to name the missing .gen/public", err)
	}
}
