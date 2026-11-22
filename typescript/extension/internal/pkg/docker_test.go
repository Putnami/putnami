package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	compileOut := filepath.Join(ws, ".putnami", "out", projPath, "~build", "compile")
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

// ---- fresh workspace image ----

const freshImageRequirement = "image-on-a-fresh-workspace"

func stageCompileOutput(t *testing.T, ws, projPath, command string, files ...string) string {
	t.Helper()
	dir := filepath.Join(ws, ".putnami", "out", projPath, "~"+command, "compile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The package step compiles the one executable --platform asks for; a stale
// build --compile output for another target must not hide it.
func TestLocateCompileOutput_TakesTheExecutableCompiledForThePlatform(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-runs-the-executable-compiled-for-the-platform")
	ws := t.TempDir()
	stageCompileOutput(t, ws, "tasks-api", "build", "serve.bundled-linux-x64")
	pkgDir := stageCompileOutput(t, ws, "tasks-api", "package", "serve.bundled-linux-arm64")

	dir, name, err := locateCompileOutput(ws, "tasks-api", "tasks-api", "linux/arm64", false)
	if err != nil {
		t.Fatalf("locateCompileOutput() error = %v", err)
	}
	if dir != pkgDir || name != "serve.bundled-linux-arm64" {
		t.Errorf("locateCompileOutput() = %q, %q; want %q, serve.bundled-linux-arm64", dir, name, pkgDir)
	}

	// The package step's own output wins over a build output for the same
	// platform: it is the one the docker step runs after.
	stageCompileOutput(t, ws, "tasks-api", "build", "serve.bundled-linux-arm64")
	if dir, _, err := locateCompileOutput(ws, "tasks-api", "tasks-api", "linux/arm64", false); err != nil || dir != pkgDir {
		t.Errorf("locateCompileOutput() = %q, %v; want the package output %q", dir, err, pkgDir)
	}
}

func TestLocateCompileOutput_FallsBackToTheBuildOutput(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-runs-the-executable-compiled-for-the-platform")
	ws := t.TempDir()
	buildDir := stageCompileOutput(t, ws, "apps/api", "build", "serve-linux-x64")
	dir, name, err := locateCompileOutput(ws, "apps/api", "api", "linux/amd64", false)
	if err != nil || dir != buildDir || name != "serve-linux-x64" {
		t.Errorf("locateCompileOutput() = %q, %q, %v; want %q, serve-linux-x64", dir, name, err, buildDir)
	}
}

func TestLocateCompileOutput_MissingExecutableNamesThePlatformAndThePaths(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-runs-the-executable-compiled-for-the-platform")
	ws := t.TempDir()
	stageCompileOutput(t, ws, "tasks-api", "package", "serve.bundled-linux-x64")
	_, _, err := locateCompileOutput(ws, "tasks-api", "tasks-api", "linux/arm64", false)
	if err == nil {
		t.Fatal("locateCompileOutput() succeeded with no arm64 executable")
	}
	for _, want := range []string{"linux/arm64", filepath.Join("~package", "compile"), filepath.Join("~build", "compile")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

func TestLocalAlias(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "a-docker-tag-with-a-repository-is-the-whole-reference")
	for tag, want := range map[string]string{
		"v1.0.0":                        "tasks-api:v1.0.0",
		"local":                         "tasks-api:local",
		"tasks-api:local":               "tasks-api:local",
		"other:dev":                     "other:dev",
		"registry.example.com/team/api": "registry.example.com/team/api",
		"localhost:5000/team/api:1.2.3": "localhost:5000/team/api:1.2.3",
	} {
		if got := localAlias("tasks-api", tag); got != want {
			t.Errorf("localAlias(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestImagePort(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-port-is-the-project-port")
	serve := func(raw string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"serve": json.RawMessage(raw)}
	}
	conf := func(files map[string]string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, "conf", name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	base := conf(map[string]string{".env.yaml": "putnami:\n  port: 8080\n"})
	both := conf(map[string]string{".env.yaml": "putnami:\n  port: 8080\n", ".env.production.yaml": "putnami:\n  port: '8443'\n"})
	local := conf(map[string]string{".env.local.yaml": "putnami:\n  port: 4000\n"})
	broken := conf(map[string]string{".env.yaml": "putnami: [\n"})
	for name, tc := range map[string]struct {
		param   int
		dir     string
		options map[string]json.RawMessage
		want    int
	}{
		"nothing declared":                  {0, t.TempDir(), nil, 3000},
		"the parameter wins":                {9000, base, serve(`{"port":8081}`), 9000},
		"the base conf file":                {0, base, serve(`{"port":8081}`), 8080},
		"the production conf file wins":     {0, both, nil, 8443},
		"another environment's file":        {0, local, nil, 3000},
		"an unparsable conf file":           {0, broken, serve(`{"port":8081}`), 8081},
		"the serve port":                    {0, t.TempDir(), serve(`{"port":8081}`), 8081},
		"a serve port as a string":          {0, t.TempDir(), serve(`{"port":"8082"}`), 8082},
		"a serve block with no port":        {0, t.TempDir(), serve(`{"watch":true}`), 3000},
		"a serve port out of range":         {0, t.TempDir(), serve(`{"port":70000}`), 3000},
		"a serve port that is not a number": {0, t.TempDir(), serve(`{"port":"${PORT}"}`), 3000},
	} {
		if got := ImagePort(tc.param, tc.dir, tc.options); got != tc.want {
			t.Errorf("%s: ImagePort() = %d, want %d", name, got, tc.want)
		}
	}
}

// The port ImagePort resolves is the one the image sets as PORT and exposes.
func TestDockerSpec_SetsAndExposesTheProjectPort(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-port-is-the-project-port")
	ws := t.TempDir()
	dir := stageCompileOutput(t, ws, "tasks-api", "package", "serve.bundled-linux-arm64")
	port := ImagePort(0, ws, map[string]json.RawMessage{"serve": json.RawMessage(`{"port":8080}`)})
	spec, err := dockerSpec("base@sha256:"+strings.Repeat("0", 64), "linux/arm64", dir, "serve.bundled-linux-arm64", "tasks-api", filepath.Join(dir, "stamp.json"), port, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Env, "PORT=8080") || !slices.Equal(spec.ExposedPorts, []string{"8080/tcp"}) {
		t.Errorf("spec env %v, exposed %v; want PORT=8080 and 8080/tcp", spec.Env, spec.ExposedPorts)
	}
}

func TestLocateCompileOutput_NodeNeedsTheBundleRecord(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", freshImageRequirement, "the-image-runs-the-executable-compiled-for-the-platform")
	ws := t.TempDir()
	stageCompileOutput(t, ws, "api", "package", "stray.js")
	buildDir := stageCompileOutput(t, ws, "api", "build", "putnami-node.json")
	dir, _, err := locateCompileOutput(ws, "api", "api", "linux/amd64", true)
	if err != nil || dir != buildDir {
		t.Errorf("locateCompileOutput() = %q, %v; want the output holding the bundle record %q", dir, err, buildDir)
	}
	if _, _, err := locateCompileOutput(t.TempDir(), "api", "api", "linux/amd64", true); err == nil || !strings.Contains(err.Error(), "compile output not found") {
		t.Errorf("locateCompileOutput() error = %v, want compile output not found", err)
	}
}
