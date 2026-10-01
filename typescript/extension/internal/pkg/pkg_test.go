package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	stdlibexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	sdkexec "go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/typescript/extension/internal/catalog"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/project"
)

// ---- resolveWorkspaceDeps ----

// mustResolveDeps resolves deps and fails the test on any resolution error.
func mustResolveDeps(t *testing.T, deps map[string]string, wsProjects map[string]bool, cat *catalog.Catalogs, versionSuffix string, release bool, wsVersion string) map[string]string {
	t.Helper()
	got, err := resolveWorkspaceDeps(deps, wsProjects, cat, versionSuffix, release, wsVersion)
	if err != nil {
		t.Fatalf("resolveWorkspaceDeps: %v", err)
	}
	return got
}

// catalogFrom builds a Catalogs view from a root-manifest JSON string.
func catalogFrom(t *testing.T, manifest string) *catalog.Catalogs {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(manifest), &raw); err != nil {
		t.Fatalf("bad manifest: %v", err)
	}
	return catalog.Parse(raw)
}

func TestResolveWorkspaceDeps_Nil(t *testing.T) {
	got := mustResolveDeps(t, nil, nil, nil, "", false, "")
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestResolveWorkspaceDeps_WorkspaceStar(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:*"}
	got := mustResolveDeps(t, deps, nil, nil, "", false, "1.0.0")
	if got["@putnami/utils"] != "1.0.0" {
		t.Errorf("workspace:* => %q, want %q", got["@putnami/utils"], "1.0.0")
	}
}

func TestResolveWorkspaceDeps_WorkspaceTilde(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:~"}
	got := mustResolveDeps(t, deps, nil, nil, "", false, "1.0.0")
	if got["@putnami/utils"] != "~1.0.0" {
		t.Errorf("workspace:~ => %q, want %q", got["@putnami/utils"], "~1.0.0")
	}
}

func TestResolveWorkspaceDeps_WorkspaceCaret(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:^"}
	got := mustResolveDeps(t, deps, nil, nil, "", false, "1.0.0")
	if got["@putnami/utils"] != "^1.0.0" {
		t.Errorf("workspace:^ => %q, want %q", got["@putnami/utils"], "^1.0.0")
	}
}

func TestResolveWorkspaceDeps_WorkspaceStarWithSuffix(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:*"}
	got := mustResolveDeps(t, deps, nil, nil, "abc1234", false, "1.0.0")
	if got["@putnami/utils"] != "1.0.0-abc1234" {
		t.Errorf("workspace:* with suffix => %q, want %q", got["@putnami/utils"], "1.0.0-abc1234")
	}
}

func TestResolveWorkspaceDeps_WorkspaceCaretWithSuffix(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:^"}
	got := mustResolveDeps(t, deps, nil, nil, "abc1234", false, "1.0.0")
	if got["@putnami/utils"] != "^1.0.0-abc1234" {
		t.Errorf("workspace:^ with suffix => %q, want %q", got["@putnami/utils"], "^1.0.0-abc1234")
	}
}

func TestResolveWorkspaceDeps_StableRelease(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:*"}
	got := mustResolveDeps(t, deps, nil, nil, "abc1234", true, "1.0.0")
	// release=true means no suffix appended
	if got["@putnami/utils"] != "1.0.0" {
		t.Errorf("stable workspace:* => %q, want %q", got["@putnami/utils"], "1.0.0")
	}
}

func TestResolveWorkspaceDeps_StableReleaseStripsPreRelease(t *testing.T) {
	wsProjects := map[string]bool{"@putnami/sdk": true}
	deps := map[string]string{"@putnami/sdk": "1.0.0-beta.1"}
	got := mustResolveDeps(t, deps, wsProjects, nil, "", true, "1.0.0")
	if got["@putnami/sdk"] != "1.0.0" {
		t.Errorf("stable non-workspace dep => %q, want %q", got["@putnami/sdk"], "1.0.0")
	}
}

func TestResolveWorkspaceDeps_NonWorkspaceDep(t *testing.T) {
	deps := map[string]string{"lodash": "^4.17.21"}
	got := mustResolveDeps(t, deps, nil, nil, "abc", false, "1.0.0")
	if got["lodash"] != "^4.17.21" {
		t.Errorf("non-workspace dep => %q, want %q", got["lodash"], "^4.17.21")
	}
}

func TestResolveWorkspaceDeps_NoWsVersionKeepsOriginal(t *testing.T) {
	deps := map[string]string{"@putnami/utils": "workspace:*"}
	got := mustResolveDeps(t, deps, nil, nil, "", false, "")
	if got["@putnami/utils"] != "workspace:*" {
		t.Errorf("no wsVersion => %q, want %q", got["@putnami/utils"], "workspace:*")
	}
}

func TestResolveWorkspaceDeps_MixedDeps(t *testing.T) {
	deps := map[string]string{
		"@putnami/utils": "workspace:^",
		"react":          "^18.0.0",
		"@putnami/web":   "workspace:*",
	}
	got := mustResolveDeps(t, deps, nil, nil, "dev123", false, "2.0.0")
	if got["@putnami/utils"] != "^2.0.0-dev123" {
		t.Errorf("utils => %q, want %q", got["@putnami/utils"], "^2.0.0-dev123")
	}
	if got["react"] != "^18.0.0" {
		t.Errorf("react => %q, want %q", got["react"], "^18.0.0")
	}
	if got["@putnami/web"] != "2.0.0-dev123" {
		t.Errorf("web => %q, want %q", got["@putnami/web"], "2.0.0-dev123")
	}
}

// ---- resolveWorkspaceDeps: catalog: protocol ----

func TestResolveWorkspaceDeps_CatalogDefault(t *testing.T) {
	// A published dep declared as catalog: must resolve to the concrete version
	// recorded in the publishing workspace catalog — never leak "catalog:".
	cat := catalogFrom(t, `{"catalog":{"@putnami/runtime":"0.1.0-9e41cd19"}}`)
	deps := map[string]string{"@putnami/runtime": "catalog:"}
	got := mustResolveDeps(t, deps, nil, cat, "abc1234", false, "1.0.0")
	if got["@putnami/runtime"] != "0.1.0-9e41cd19" {
		t.Errorf("catalog: => %q, want %q", got["@putnami/runtime"], "0.1.0-9e41cd19")
	}
}

func TestResolveWorkspaceDeps_CatalogNamed(t *testing.T) {
	cat := catalogFrom(t, `{"catalogs":{"framework":{"@putnami/web":"1.2.3"}}}`)
	deps := map[string]string{"@putnami/web": "catalog:framework"}
	got := mustResolveDeps(t, deps, nil, cat, "", true, "2.0.0")
	if got["@putnami/web"] != "1.2.3" {
		t.Errorf("catalog:framework => %q, want %q", got["@putnami/web"], "1.2.3")
	}
}

func TestResolveWorkspaceDeps_CatalogPointingAtWorkspace(t *testing.T) {
	// A catalog entry may itself hold a workspace: spec (framework packages
	// aligned through a catalog); that must resolve through to the workspace
	// version, suffix and all.
	cat := catalogFrom(t, `{"catalog":{"@putnami/utils":"workspace:^"}}`)
	deps := map[string]string{"@putnami/utils": "catalog:"}
	got := mustResolveDeps(t, deps, nil, cat, "dev123", false, "2.0.0")
	if got["@putnami/utils"] != "^2.0.0-dev123" {
		t.Errorf("catalog:→workspace:^ => %q, want %q", got["@putnami/utils"], "^2.0.0-dev123")
	}
}

func TestResolveWorkspaceDeps_CatalogUnresolvedFailsLoud(t *testing.T) {
	// The bug this fixes: a catalog: spec with no matching catalog entry must
	// abort the publish rather than ship a tarball carrying "catalog:".
	deps := map[string]string{"@putnami/runtime": "catalog:"}

	if _, err := resolveWorkspaceDeps(deps, nil, nil, "abc", false, "1.0.0"); err == nil {
		t.Error("expected error when no catalog is present, got nil")
	}

	cat := catalogFrom(t, `{"catalog":{"@putnami/web":"1.0.0"}}`)
	if _, err := resolveWorkspaceDeps(deps, nil, cat, "abc", false, "1.0.0"); err == nil {
		t.Error("expected error when the catalog lacks the entry, got nil")
	}
}

func TestResolvePublishDeps_PropagatesCatalogError(t *testing.T) {
	pkg := &project.PackageJSON{
		Dependencies: map[string]string{"@putnami/runtime": "catalog:"},
	}
	if err := resolvePublishDeps(pkg, nil, nil, "abc", false, "1.0.0"); err == nil {
		t.Error("expected resolvePublishDeps to propagate the catalog resolution error")
	}
}

func TestResolvePublishDeps_ResolvesAllMaps(t *testing.T) {
	cat := catalogFrom(t, `{"catalog":{"@putnami/runtime":"0.1.0-abc","@putnami/web":"1.2.3"}}`)
	pkg := &project.PackageJSON{
		Dependencies:         map[string]string{"@putnami/runtime": "catalog:"},
		PeerDependencies:     map[string]string{"@putnami/web": "catalog:"},
		OptionalDependencies: map[string]string{"react": "^19.0.0"},
	}
	if err := resolvePublishDeps(pkg, nil, cat, "dev", false, "2.0.0"); err != nil {
		t.Fatalf("resolvePublishDeps: %v", err)
	}
	if pkg.Dependencies["@putnami/runtime"] != "0.1.0-abc" {
		t.Errorf("dependencies not resolved: %v", pkg.Dependencies)
	}
	if pkg.PeerDependencies["@putnami/web"] != "1.2.3" {
		t.Errorf("peerDependencies not resolved: %v", pkg.PeerDependencies)
	}
	if pkg.OptionalDependencies["react"] != "^19.0.0" {
		t.Errorf("optionalDependencies mutated: %v", pkg.OptionalDependencies)
	}
}

// ---- inheritWorkspaceFields ----

func TestInheritWorkspaceFields_CopiesMissingFields(t *testing.T) {
	pkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"name": json.RawMessage(`"my-pkg"`),
		},
	}
	wsPkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"author":  json.RawMessage(`"Putnami"`),
			"license": json.RawMessage(`"MIT"`),
		},
	}

	inheritWorkspaceFields(pkg, wsPkg)

	if string(pkg.Raw["author"]) != `"Putnami"` {
		t.Errorf("expected author to be inherited, got %s", string(pkg.Raw["author"]))
	}
	if string(pkg.Raw["license"]) != `"MIT"` {
		t.Errorf("expected license to be inherited, got %s", string(pkg.Raw["license"]))
	}
}

func TestInheritWorkspaceFields_DoesNotOverride(t *testing.T) {
	pkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"license": json.RawMessage(`"Apache-2.0"`),
		},
	}
	wsPkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"license": json.RawMessage(`"MIT"`),
		},
	}

	inheritWorkspaceFields(pkg, wsPkg)

	if string(pkg.Raw["license"]) != `"Apache-2.0"` {
		t.Errorf("expected license not to be overridden, got %s", string(pkg.Raw["license"]))
	}
}

func TestInheritWorkspaceFields_NilRaw(t *testing.T) {
	pkg := &project.PackageJSON{Raw: nil}
	wsPkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"author": json.RawMessage(`"Putnami"`),
		},
	}

	// Should not panic
	inheritWorkspaceFields(pkg, wsPkg)
}

func TestInheritWorkspaceFields_AllFields(t *testing.T) {
	fields := []string{"author", "license", "repository", "bugs", "homepage", "funding", "engines", "packageManager"}
	wsPkgRaw := make(map[string]json.RawMessage)
	for _, f := range fields {
		wsPkgRaw[f] = json.RawMessage(`"ws-` + f + `"`)
	}
	wsPkg := &project.PackageJSON{Raw: wsPkgRaw}
	pkg := &project.PackageJSON{Raw: map[string]json.RawMessage{}}

	inheritWorkspaceFields(pkg, wsPkg)

	for _, f := range fields {
		if _, ok := pkg.Raw[f]; !ok {
			t.Errorf("expected field %q to be inherited", f)
		}
	}
}

// ---- findBuildOutput ----

func TestFindBuildOutput_PackagePath(t *testing.T) {
	dir := t.TempDir()
	pkgPath := filepath.Join(dir, ".putnami", "out", "my-pkg", "package")
	os.MkdirAll(pkgPath, 0755)

	got := findBuildOutput(dir, "my-pkg")
	if got != pkgPath {
		t.Errorf("findBuildOutput() = %q, want %q", got, pkgPath)
	}
}

func TestFindBuildOutput_BuildPath(t *testing.T) {
	dir := t.TempDir()
	buildPath := filepath.Join(dir, ".putnami", "out", "my-pkg", "build")
	os.MkdirAll(buildPath, 0755)

	got := findBuildOutput(dir, "my-pkg")
	if got != buildPath {
		t.Errorf("findBuildOutput() = %q, want %q", got, buildPath)
	}
}

func TestFindBuildOutput_CachePath(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".putnami", "projects", "my-pkg",
		"@putnami-typescript", "build~transpile", "latest", "output")
	os.MkdirAll(cachePath, 0755)

	got := findBuildOutput(dir, "my-pkg")
	if got != cachePath {
		t.Errorf("findBuildOutput() = %q, want %q", got, cachePath)
	}
}

func TestFindBuildOutput_CachePathSanitizesName(t *testing.T) {
	dir := t.TempDir()
	// @putnami/utils -> @putnami-utils (/ replaced with -, @ kept)
	cachePath := filepath.Join(dir, ".putnami", "projects", "@putnami-utils",
		"@putnami-typescript", "build~transpile", "latest", "output")
	os.MkdirAll(cachePath, 0755)

	got := findBuildOutput(dir, "@putnami/utils")
	if got != cachePath {
		t.Errorf("findBuildOutput() = %q, want %q", got, cachePath)
	}
}

func TestFindBuildOutput_NotFound(t *testing.T) {
	dir := t.TempDir()
	got := findBuildOutput(dir, "nonexistent")
	if got != "" {
		t.Errorf("findBuildOutput() = %q, want empty", got)
	}
}

func TestFindBuildOutput_PackagePreferred(t *testing.T) {
	dir := t.TempDir()
	// Create both package and build paths; package should be preferred
	pkgPath := filepath.Join(dir, ".putnami", "out", "my-pkg", "package")
	buildPath := filepath.Join(dir, ".putnami", "out", "my-pkg", "build")
	os.MkdirAll(pkgPath, 0755)
	os.MkdirAll(buildPath, 0755)

	got := findBuildOutput(dir, "my-pkg")
	if got != pkgPath {
		t.Errorf("findBuildOutput() = %q, want package path %q", got, pkgPath)
	}
}

// ---- findBinaryForPlatform ----

func TestFindBinaryForPlatform_LinuxAmd64(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app-linux-x64"), []byte("binary"), 0755)

	got, err := findBinaryForPlatform(dir, "linux/amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "app-linux-x64" {
		t.Errorf("findBinaryForPlatform() = %q, want %q", got, "app-linux-x64")
	}
}

func TestFindBinaryForPlatform_LinuxArm64(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app-linux-arm64"), []byte("binary"), 0755)

	got, err := findBinaryForPlatform(dir, "linux/arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "app-linux-arm64" {
		t.Errorf("findBinaryForPlatform() = %q, want %q", got, "app-linux-arm64")
	}
}

func TestFindBinaryForPlatform_InvalidFormat(t *testing.T) {
	dir := t.TempDir()
	_, err := findBinaryForPlatform(dir, "invalid")
	if err == nil {
		t.Error("expected error for invalid platform format")
	}
	if !strings.Contains(err.Error(), "invalid platform format") {
		t.Errorf("error = %q, want to contain 'invalid platform format'", err.Error())
	}
}

func TestFindBinaryForPlatform_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app-darwin-x64"), []byte("binary"), 0755)

	_, err := findBinaryForPlatform(dir, "linux/amd64")
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestFindBinaryForPlatform_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	// Create a directory with matching suffix — should not be returned
	os.MkdirAll(filepath.Join(dir, "app-linux-x64"), 0755)

	_, err := findBinaryForPlatform(dir, "linux/amd64")
	if err == nil {
		t.Error("expected error when only matching entry is a directory")
	}
}

// ---- dockerSpec ----

func TestDockerSpec_Basic(t *testing.T) {
	spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", "/out", "app-linux-x64", "my-app", "/out/stamp.json", 8080, false)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(spec.BaseRef, "@sha256:") {
		t.Errorf("base ref must be digest-pinned for reproducible digests, got %q", spec.BaseRef)
	}
	// Without assets: binary layer + stamp layer.
	if len(spec.Layers) != 2 {
		t.Fatalf("expected binary + stamp layers, got %+v", spec.Layers)
	}
	binary := spec.Layers[0].Files[0]
	if binary.Path != "/app/my-app" || binary.Mode != 0o755 {
		t.Errorf("binary file = %+v, want /app/my-app mode 0755", binary)
	}
	stamp := spec.Layers[1].Files[0]
	if stamp.Path != stampImagePath || stamp.Mode != 0o644 {
		t.Errorf("stamp file = %+v, want %s mode 0644", stamp, stampImagePath)
	}
	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/app/my-app" {
		t.Errorf("entrypoint = %v", spec.Entrypoint)
	}
	if spec.WorkingDir != "/app" {
		t.Errorf("workingDir = %q", spec.WorkingDir)
	}
	wantEnv := []string{"NODE_ENV=production", "PORT=8080", "PWD=/app"}
	if strings.Join(spec.Env, ";") != strings.Join(wantEnv, ";") {
		t.Errorf("env = %v, want %v", spec.Env, wantEnv)
	}
	if len(spec.ExposedPorts) != 1 || spec.ExposedPorts[0] != "8080/tcp" {
		t.Errorf("exposedPorts = %v", spec.ExposedPorts)
	}
}

func TestDockerSpec_DefaultAndNegativePort(t *testing.T) {
	for _, port := range []int{0, -1} {
		spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", "/out", "bin", "app", "/out/stamp.json", port, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(spec.Env, ";"), "PORT=3000") {
			t.Errorf("port %d: env = %v, want PORT=3000", port, spec.Env)
		}
		if len(spec.ExposedPorts) != 1 || spec.ExposedPorts[0] != "3000/tcp" {
			t.Errorf("port %d: exposedPorts = %v, want 3000/tcp", port, spec.ExposedPorts)
		}
	}
}

func TestDockerSpec_WithAssets(t *testing.T) {
	dir := t.TempDir()
	assetsDir := filepath.Join(dir, ".gen", "public")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "style.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", dir, "bin", "app", filepath.Join(dir, "stamp.json"), 3000, true)
	if err != nil {
		t.Fatal(err)
	}
	// Binary, assets, stamp — the stamp last, after the asset layer.
	if len(spec.Layers) != 3 {
		t.Fatalf("expected binary + assets + stamp layers, got %d", len(spec.Layers))
	}
	asset := spec.Layers[1].Files[0]
	if asset.Path != "/app/.gen/public/style.css" || asset.Mode != 0o644 {
		t.Errorf("asset file = %+v", asset)
	}
	if spec.Layers[2].Files[0].Path != stampImagePath {
		t.Errorf("expected stamp as the last layer, got %+v", spec.Layers[2].Files)
	}
}

func TestDockerSpec_NoGitBytes(t *testing.T) {
	spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", "/out", "bin", "app", "/out/stamp.json", 3000, false)
	if err != nil {
		t.Fatal(err)
	}
	// No git-derived bytes: nothing version-shaped may reach the spec, or the
	// image digest would change per release id for identical content.
	canonical := spec.CanonicalString()
	for _, forbidden := range []string{"0.0.0", "revision", "branch", "org.opencontainers.image"} {
		if strings.Contains(canonical, forbidden) {
			t.Errorf("spec must not carry %q, got: %s", forbidden, canonical)
		}
	}
}

func TestWriteContentStamp_Deterministic(t *testing.T) {
	dir := t.TempDir()

	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	if err := writeContentStamp(pathA, "my-app", "abc123def456"); err != nil {
		t.Fatalf("writeContentStamp: %v", err)
	}
	if err := writeContentStamp(pathB, "my-app", "abc123def456"); err != nil {
		t.Fatalf("writeContentStamp: %v", err)
	}

	a, _ := os.ReadFile(pathA)
	b, _ := os.ReadFile(pathB)
	if string(a) != string(b) {
		t.Errorf("expected byte-identical stamps, got:\n%s\nvs:\n%s", a, b)
	}

	var stamp map[string]any
	if err := json.Unmarshal(a, &stamp); err != nil {
		t.Fatalf("stamp is not valid JSON: %v", err)
	}
	if stamp["name"] != "my-app" || stamp["contentHash"] != "abc123def456" {
		t.Errorf("unexpected stamp content: %v", stamp)
	}
	// Git-derived fields must never reach the image: the stamp would change
	// per commit and break digest stability.
	for _, forbidden := range []string{"version", "sha", "branch", "suffix", "isDirty", "buildTime"} {
		if _, ok := stamp[forbidden]; ok {
			t.Errorf("stamp must not contain %q, got: %v", forbidden, stamp)
		}
	}
}

func TestDockerSpec_HashStableAndInputSensitive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app-linux-x64"), []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	stampPath := filepath.Join(dir, contentStampFile)
	hashFor := func(port int, hasAssets bool) string {
		t.Helper()
		spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", dir, "app-linux-x64", "app", stampPath, port, hasAssets)
		if err != nil {
			t.Fatal(err)
		}
		full, err := oci.ContentHash(spec, stampImagePath)
		if err != nil {
			t.Fatalf("ContentHash: %v", err)
		}
		return full[:12]
	}

	hashA := hashFor(3000, false)
	if hashB := hashFor(3000, false); hashB != hashA {
		t.Errorf("expected stable hash for identical inputs, got %q vs %q", hashA, hashB)
	}
	if len(hashA) != 12 {
		t.Errorf("expected 12-char hash, got %q", hashA)
	}

	// The stamp content embeds this very hash, so it must not feed back into
	// it — writing the stamp must not change the hash.
	if err := writeContentStamp(stampPath, "app", hashA); err != nil {
		t.Fatal(err)
	}
	if hashStamped := hashFor(3000, false); hashStamped != hashA {
		t.Error("expected stamp content to be excluded from the content hash")
	}

	// Binary change must change the hash.
	if err := os.WriteFile(filepath.Join(dir, "app-linux-x64"), []byte("binary-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	hashC := hashFor(3000, false)
	if hashC == hashA {
		t.Error("expected hash to change when binary content changes")
	}

	// Spec change (e.g. port) must change the hash.
	if hashD := hashFor(8080, false); hashD == hashC {
		t.Error("expected hash to change when the spec changes")
	}
}

func TestDockerSpec_AssetsAffectHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app-linux-x64"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	assetsDir := filepath.Join(dir, ".gen", "public")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "style.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stampPath := filepath.Join(dir, contentStampFile)
	hashFor := func() string {
		t.Helper()
		spec, err := dockerSpec(dockerBaseImagePinned, "linux/amd64", dir, "app-linux-x64", "app", stampPath, 3000, true)
		if err != nil {
			t.Fatal(err)
		}
		full, err := oci.ContentHash(spec, stampImagePath)
		if err != nil {
			t.Fatalf("ContentHash: %v", err)
		}
		return full[:12]
	}

	hashA := hashFor()

	if err := os.WriteFile(filepath.Join(assetsDir, "style.css"), []byte("body{color:red}"), 0o644); err != nil {
		t.Fatal(err)
	}
	hashB := hashFor()
	if hashB == hashA {
		t.Error("expected hash to change when asset content changes")
	}

	// Renaming an asset must change the hash even with identical bytes.
	if err := os.Rename(filepath.Join(assetsDir, "style.css"), filepath.Join(assetsDir, "main.css")); err != nil {
		t.Fatal(err)
	}
	if hashC := hashFor(); hashC == hashB {
		t.Error("expected hash to change when asset path changes")
	}
}

// ---- rewriteTSPathsToJS ----

func TestTsPathToJS_TS(t *testing.T) {
	if got := tsPathToJS("./src/index.ts"); got != "./src/index.js" {
		t.Errorf("tsPathToJS(.ts) = %q, want %q", got, "./src/index.js")
	}
}

func TestTsPathToJS_TSX(t *testing.T) {
	if got := tsPathToJS("./src/app.tsx"); got != "./src/app.js" {
		t.Errorf("tsPathToJS(.tsx) = %q, want %q", got, "./src/app.js")
	}
}

func TestTsPathToJS_JS(t *testing.T) {
	if got := tsPathToJS("./src/index.js"); got != "./src/index.js" {
		t.Errorf("tsPathToJS(.js) = %q, want %q", got, "./src/index.js")
	}
}

func TestTsPathToJS_Empty(t *testing.T) {
	if got := tsPathToJS(""); got != "" {
		t.Errorf("tsPathToJS('') = %q, want empty", got)
	}
}

func TestRewriteTSPathsToJS_MainAndExports(t *testing.T) {
	pkg := &project.PackageJSON{
		Main:    "src/index.ts",
		Exports: json.RawMessage(`{".":{"default":"./src/index.ts","browser":"./src/index.browser.ts"},"./testing":"./src/testing/index.ts"}`),
	}

	rewriteTSPathsToJS(pkg)

	if pkg.Main != "src/index.js" {
		t.Errorf("Main = %q, want %q", pkg.Main, "src/index.js")
	}

	var exports map[string]any
	json.Unmarshal(pkg.Exports, &exports)

	dot := exports["."].(map[string]any)
	if dot["default"] != "./src/index.js" {
		t.Errorf("exports['.']['default'] = %q, want %q", dot["default"], "./src/index.js")
	}
	if dot["browser"] != "./src/index.browser.js" {
		t.Errorf("exports['.']['browser'] = %q, want %q", dot["browser"], "./src/index.browser.js")
	}
	if exports["./testing"] != "./src/testing/index.js" {
		t.Errorf("exports['./testing'] = %q, want %q", exports["./testing"], "./src/testing/index.js")
	}
}

func TestRewriteTSPathsToJS_NilExports(t *testing.T) {
	pkg := &project.PackageJSON{Main: "src/main.ts"}
	rewriteTSPathsToJS(pkg)
	if pkg.Main != "src/main.js" {
		t.Errorf("Main = %q, want %q", pkg.Main, "src/main.js")
	}
}

func TestRewriteTSPathsToJS_BrowserMapKeysAndValues(t *testing.T) {
	// The published tree contains only .js files, so a browser map left on .ts
	// paths redirects nothing — it points at files that were never shipped.
	pkg := &project.PackageJSON{
		Main: "src/index.ts",
		Raw: map[string]json.RawMessage{
			"browser": json.RawMessage(`{"./src/client/document/document.helper.ts":"./src/client/document/document.helper.browser.ts","./src/server/only.tsx":false}`),
		},
	}

	rewriteTSPathsToJS(pkg)

	var browser map[string]any
	if err := json.Unmarshal(pkg.Raw["browser"], &browser); err != nil {
		t.Fatalf("browser field is not an object: %v", err)
	}
	got, ok := browser["./src/client/document/document.helper.js"]
	if !ok {
		t.Fatalf("browser map key was not rewritten to .js: %v", browser)
	}
	if got != "./src/client/document/document.helper.browser.js" {
		t.Errorf("browser map value = %v, want ./src/client/document/document.helper.browser.js", got)
	}
	stub, ok := browser["./src/server/only.js"]
	if !ok {
		t.Fatalf("browser map .tsx key was not rewritten to .js: %v", browser)
	}
	if stub != false {
		t.Errorf("browser map false stub = %v, want false", stub)
	}
}

func TestRewriteTSPathsToJS_BrowserStringField(t *testing.T) {
	pkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{"browser": json.RawMessage(`"./src/index.browser.ts"`)},
	}

	rewriteTSPathsToJS(pkg)

	var browser string
	if err := json.Unmarshal(pkg.Raw["browser"], &browser); err != nil {
		t.Fatalf("browser field is not a string: %v", err)
	}
	if browser != "./src/index.browser.js" {
		t.Errorf("browser = %q, want %q", browser, "./src/index.browser.js")
	}
}

func TestRewriteTSPathsToJS_NoBrowserField(t *testing.T) {
	pkg := &project.PackageJSON{
		Main: "src/index.ts",
		Raw:  map[string]json.RawMessage{"name": json.RawMessage(`"test"`)},
	}

	rewriteTSPathsToJS(pkg)

	if _, ok := pkg.Raw["browser"]; ok {
		t.Error("rewrite must not invent a browser field")
	}
}

func TestRewriteTSPathsToJS_BinField(t *testing.T) {
	pkg := &project.PackageJSON{
		Main: "src/index.ts",
		Bin:  json.RawMessage(`{"putnami-react-generate":"./bin/generate.ts"}`),
	}

	rewriteTSPathsToJS(pkg)

	var bin map[string]string
	json.Unmarshal(pkg.Bin, &bin)

	if bin["putnami-react-generate"] != "./bin/generate.js" {
		t.Errorf("bin['putnami-react-generate'] = %q, want %q", bin["putnami-react-generate"], "./bin/generate.js")
	}
}

// ---- copyHookFiles ----

func TestCopyHookFiles_CopiesManifestOnly(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	// Create putnami.extension.json
	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{"hooks":{}}`), 0644)

	// Create bin/ with .ts files (should NOT be copied — bin files are now
	// transpiled as entrypoints and included in the lib output)
	os.MkdirAll(filepath.Join(projDir, "bin"), 0755)
	os.WriteFile(filepath.Join(projDir, "bin", "generate.ts"), []byte("console.log('gen')"), 0644)

	copyHookFiles(projDir, outDir, "1.0.0")

	// Manifest should be copied
	if !project.FileExists(filepath.Join(outDir, "putnami.extension.json")) {
		t.Error("expected putnami.extension.json to be copied")
	}

	// bin/*.ts should NOT be copied (they come from transpile output)
	if project.FileExists(filepath.Join(outDir, "bin", "generate.ts")) {
		t.Error("expected bin/generate.ts to NOT be copied — bin files are transpiled as entrypoints")
	}
}

func TestCopyHookFiles_StampsVersion(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{"hooks":{}}`), 0644)

	copyHookFiles(projDir, outDir, "2.3.4")

	data, err := os.ReadFile(filepath.Join(outDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read output manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse output manifest: %v", err)
	}
	if manifest["version"] != "2.3.4" {
		t.Errorf("version = %q, want %q", manifest["version"], "2.3.4")
	}
}

func TestCopyHookFiles_NoManifest(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	// No putnami.extension.json — nothing should be copied
	os.MkdirAll(filepath.Join(projDir, "bin"), 0755)
	os.WriteFile(filepath.Join(projDir, "bin", "generate.ts"), []byte("gen"), 0644)

	copyHookFiles(projDir, outDir, "1.0.0")

	if project.FileExists(filepath.Join(outDir, "bin", "generate.ts")) {
		t.Error("expected no bin files copied without manifest")
	}
}

func TestCopyHookFiles_ManifestButNoBinDir(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	// Only manifest, no bin/ directory
	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{}`), 0644)

	copyHookFiles(projDir, outDir, "1.0.0")

	// Manifest should still be copied
	if !project.FileExists(filepath.Join(outDir, "putnami.extension.json")) {
		t.Error("expected putnami.extension.json to be copied")
	}
}

func TestCopyHookFiles_InvalidJSONManifest(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte("not valid json"), 0644)

	// A malformed manifest must fail the package job: it can never be
	// published, because every consumer CLI would fail to unmarshal it and
	// silently skip the whole extension. It must not be staged as-is either.
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err == nil {
		t.Fatal("expected copyHookFiles to fail on an unparseable manifest")
	}
	if project.FileExists(filepath.Join(outDir, "putnami.extension.json")) {
		t.Error("an unparseable manifest must not be staged for publishing")
	}
}

// ---- resolveDockerVersion ----

func TestResolveDockerVersion_WithVersionAndInfo(t *testing.T) {
	info := &git.VersionInfo{SHA: "abc1234", Suffix: "abc1234"}
	got := resolveDockerVersion("1.0.0", info)
	if got != "1.0.0-abc1234" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "1.0.0-abc1234")
	}
}

func TestResolveDockerVersion_WithVersionAndDirtySuffix(t *testing.T) {
	info := &git.VersionInfo{SHA: "abc1234", Suffix: "abc1234-d5e6f7a"}
	got := resolveDockerVersion("2.0.0", info)
	if got != "2.0.0-abc1234-d5e6f7a" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "2.0.0-abc1234-d5e6f7a")
	}
}

func TestResolveDockerVersion_NoSuffix(t *testing.T) {
	info := &git.VersionInfo{SHA: "", Suffix: ""}
	got := resolveDockerVersion("1.0.0", info)
	if got != "1.0.0" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "1.0.0")
	}
}

func TestResolveDockerVersion_EmptySuffixFallsBackToSHA(t *testing.T) {
	info := &git.VersionInfo{SHA: "abc1234", Suffix: ""}
	got := resolveDockerVersion("1.0.0", info)
	if got != "1.0.0-abc1234" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "1.0.0-abc1234")
	}
}

func TestResolveDockerVersion_NilVersionInfo(t *testing.T) {
	got := resolveDockerVersion("1.0.0", nil)
	if got != "1.0.0" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "1.0.0")
	}
}

func TestResolveDockerVersion_EmptyWsVersion(t *testing.T) {
	info := &git.VersionInfo{SHA: "abc", Suffix: "abc"}
	got := resolveDockerVersion("", info)
	if got != "0.0.0-abc" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "0.0.0-abc")
	}
}

func TestResolveDockerVersion_EmptyWsVersionNilInfo(t *testing.T) {
	got := resolveDockerVersion("", nil)
	if got != "0.0.0" {
		t.Errorf("resolveDockerVersion() = %q, want %q", got, "0.0.0")
	}
}

// ---- resolveWorkspaceConfig ----

func TestResolveWorkspaceConfig_PrefersModern(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	got := resolveWorkspaceConfig(dir)
	if !strings.Contains(got, "putnami.workspace.json") {
		t.Errorf("expected putnami.workspace.json preferred, got %q", got)
	}
}

func TestResolveWorkspaceConfig_FallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	got := resolveWorkspaceConfig(dir)
	if !strings.Contains(got, ".putnamirc.json") {
		t.Errorf("expected .putnamirc.json fallback, got %q", got)
	}
}

func TestResolveWorkspaceConfig_NeitherExists(t *testing.T) {
	dir := t.TempDir()
	got := resolveWorkspaceConfig(dir)
	if !strings.Contains(got, "putnami.workspace.json") {
		t.Errorf("expected default putnami.workspace.json path, got %q", got)
	}
}

// ---- resolveProjectConfig ----

func TestResolveProjectConfig_PrefersModern(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	got := resolveProjectConfig(dir)
	if !strings.Contains(got, "putnami.json") || strings.Contains(got, ".putnamirc") {
		t.Errorf("expected putnami.json preferred, got %q", got)
	}
}

func TestResolveProjectConfig_FallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	got := resolveProjectConfig(dir)
	if !strings.Contains(got, ".putnamirc.json") {
		t.Errorf("expected .putnamirc.json fallback, got %q", got)
	}
}

func TestResolveProjectConfig_NeitherExists(t *testing.T) {
	dir := t.TempDir()
	got := resolveProjectConfig(dir)
	if !strings.Contains(got, "putnami.json") {
		t.Errorf("expected default putnami.json path, got %q", got)
	}
}

// ---- setGitInfoRelease ----

func TestSetGitInfoRelease_WithVersionInfo(t *testing.T) {
	pkg := &project.PackageJSON{Raw: map[string]json.RawMessage{}}
	info := &git.VersionInfo{Branch: "main", SHA: "abc1234", IsDirty: false}

	setGitInfoRelease(pkg, info, true)

	var gitInfo map[string]any
	json.Unmarshal(pkg.Raw["gitInfo"], &gitInfo)

	if gitInfo["release"] != true {
		t.Errorf("expected release=true, got %v", gitInfo["release"])
	}
	if gitInfo["branch"] != "main" {
		t.Errorf("expected branch=main, got %v", gitInfo["branch"])
	}
	if gitInfo["sha"] != "abc1234" {
		t.Errorf("expected sha=abc1234, got %v", gitInfo["sha"])
	}
	if gitInfo["isDirty"] != false {
		t.Errorf("expected isDirty=false, got %v", gitInfo["isDirty"])
	}
	if _, ok := gitInfo["buildTime"]; !ok {
		t.Error("expected buildTime to be set")
	}
}

func TestSetGitInfoRelease_NilVersionInfo(t *testing.T) {
	pkg := &project.PackageJSON{Raw: map[string]json.RawMessage{}}

	setGitInfoRelease(pkg, nil, false)

	var gitInfo map[string]any
	json.Unmarshal(pkg.Raw["gitInfo"], &gitInfo)

	if gitInfo["release"] != false {
		t.Errorf("expected release=false, got %v", gitInfo["release"])
	}
	// Should not have branch/sha/isDirty when versionInfo is nil
	if _, ok := gitInfo["branch"]; ok {
		t.Error("expected no branch when versionInfo is nil")
	}
}

func TestSetGitInfoRelease_NilRaw(t *testing.T) {
	pkg := &project.PackageJSON{Raw: nil}
	setGitInfoRelease(pkg, nil, true)

	if pkg.Raw == nil {
		t.Error("expected Raw to be initialized")
	}
	if _, ok := pkg.Raw["gitInfo"]; !ok {
		t.Error("expected gitInfo to be set")
	}
}

// ---- deleteRawField ----

func TestDeleteRawField_ExistingField(t *testing.T) {
	pkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{
			"name":    json.RawMessage(`"test"`),
			"scripts": json.RawMessage(`{"build":"tsc"}`),
		},
	}

	deleteRawField(pkg, "scripts")

	if _, ok := pkg.Raw["scripts"]; ok {
		t.Error("expected scripts to be deleted")
	}
	if _, ok := pkg.Raw["name"]; !ok {
		t.Error("expected name to be preserved")
	}
}

func TestDeleteRawField_NonexistentField(t *testing.T) {
	pkg := &project.PackageJSON{
		Raw: map[string]json.RawMessage{"name": json.RawMessage(`"test"`)},
	}

	// Should not panic
	deleteRawField(pkg, "nonexistent")
}

func TestDeleteRawField_NilRaw(t *testing.T) {
	pkg := &project.PackageJSON{Raw: nil}

	// Should not panic
	deleteRawField(pkg, "anything")
}

// ---- copyReadme ----

func TestCopyReadme_READMEmd(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(projDir, "README.md"), []byte("# Hello"), 0644)
	copyReadme(projDir, outDir)

	data, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to be copied: %v", err)
	}
	if string(data) != "# Hello" {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestCopyReadme_LowercaseReadme(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(projDir, "readme.md"), []byte("# hello"), 0644)
	copyReadme(projDir, outDir)

	data, err := os.ReadFile(filepath.Join(outDir, "readme.md"))
	if err != nil {
		t.Fatalf("expected readme.md to be copied: %v", err)
	}
	if string(data) != "# hello" {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestCopyReadme_NoReadme(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()

	copyReadme(projDir, outDir)

	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Errorf("expected no files copied, got %d", len(entries))
	}
}

func TestCopyReadme_PrefersUppercase(t *testing.T) {
	// On case-insensitive filesystems (macOS), README.md and readme.md are the
	// same file, so this test cannot verify preference order.
	projDir := t.TempDir()
	upper := filepath.Join(projDir, "README.md")
	lower := filepath.Join(projDir, "readme.md")
	os.WriteFile(upper, []byte("UPPER"), 0644)
	os.WriteFile(lower, []byte("lower"), 0644)
	upperData, _ := os.ReadFile(upper)
	if string(upperData) != "UPPER" {
		return
	}

	outDir := t.TempDir()
	copyReadme(projDir, outDir)

	// Should prefer README.md
	data, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to be copied: %v", err)
	}
	if string(data) != "UPPER" {
		t.Errorf("expected uppercase README to be preferred, got %q", string(data))
	}
}

// ---- copyLicenseFiles ----

func TestCopyLicenseFiles_SingleLicense(t *testing.T) {
	wsDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(wsDir, "LICENSE"), []byte("MIT License"), 0644)
	os.WriteFile(filepath.Join(wsDir, "other.txt"), []byte("not copied"), 0644)

	copyLicenseFiles(wsDir, outDir)

	data, err := os.ReadFile(filepath.Join(outDir, "LICENSE"))
	if err != nil {
		t.Fatalf("expected LICENSE to be copied: %v", err)
	}
	if string(data) != "MIT License" {
		t.Errorf("unexpected content: %q", string(data))
	}

	if project.FileExists(filepath.Join(outDir, "other.txt")) {
		t.Error("expected other.txt not to be copied")
	}
}

func TestCopyLicenseFiles_MultipleLicenses(t *testing.T) {
	wsDir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(wsDir, "LICENSE"), []byte("MIT"), 0644)
	os.WriteFile(filepath.Join(wsDir, "LICENSE.md"), []byte("MIT MD"), 0644)

	copyLicenseFiles(wsDir, outDir)

	if !project.FileExists(filepath.Join(outDir, "LICENSE")) {
		t.Error("expected LICENSE to be copied")
	}
	if !project.FileExists(filepath.Join(outDir, "LICENSE.md")) {
		t.Error("expected LICENSE.md to be copied")
	}
}

func TestCopyLicenseFiles_NoLicense(t *testing.T) {
	wsDir := t.TempDir()
	outDir := t.TempDir()

	copyLicenseFiles(wsDir, outDir)

	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Errorf("expected no files copied, got %d", len(entries))
	}
}

// ---- loadWorkspaceProjectNames ----

func TestLoadWorkspaceProjectNames_ValidWorkspace(t *testing.T) {
	wsDir := t.TempDir()

	// Create workspace config
	wsConfig := `{"projects":["packages/utils","packages/runtime"]}`
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(wsConfig), 0644)

	// Create project directories with package.json
	utilsDir := filepath.Join(wsDir, "packages", "utils")
	os.MkdirAll(utilsDir, 0755)
	os.WriteFile(filepath.Join(utilsDir, "package.json"), []byte(`{"name":"@putnami/utils"}`), 0644)

	runtimeDir := filepath.Join(wsDir, "packages", "runtime")
	os.MkdirAll(runtimeDir, 0755)
	os.WriteFile(filepath.Join(runtimeDir, "package.json"), []byte(`{"name":"@putnami/runtime"}`), 0644)

	names := loadWorkspaceProjectNames(wsDir)
	if !names["@putnami/utils"] {
		t.Error("expected @putnami/utils in names")
	}
	if !names["@putnami/runtime"] {
		t.Error("expected @putnami/runtime in names")
	}
}

func TestLoadWorkspaceProjectNames_NoWorkspaceConfig(t *testing.T) {
	wsDir := t.TempDir()
	names := loadWorkspaceProjectNames(wsDir)
	if len(names) != 0 {
		t.Errorf("expected empty names, got %d", len(names))
	}
}

func TestLoadWorkspaceProjectNames_FallsBackToPutnamiRC(t *testing.T) {
	wsDir := t.TempDir()

	wsConfig := `{"projects":["pkg"]}`
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(wsConfig), 0644)

	// Project with putnami.json instead of package.json
	pkgDir := filepath.Join(wsDir, "pkg")
	os.MkdirAll(pkgDir, 0755)
	os.WriteFile(filepath.Join(pkgDir, "putnami.json"), []byte(`{"name":"my-go-pkg"}`), 0644)

	names := loadWorkspaceProjectNames(wsDir)
	if !names["my-go-pkg"] {
		t.Error("expected my-go-pkg in names from putnami.json")
	}
}

func TestLoadWorkspaceProjectNames_PathTraversalProtection(t *testing.T) {
	wsDir := t.TempDir()

	// Workspace config with path traversal attempt
	wsConfig := `{"projects":["../../outside"]}`
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(wsConfig), 0644)

	names := loadWorkspaceProjectNames(wsDir)
	if len(names) != 0 {
		t.Errorf("expected no names for path traversal, got %v", names)
	}
}

// ---- rewriteExportsValue ----

func TestRewriteExportsValue_ArrayType(t *testing.T) {
	// Test default case — non-string, non-map values pass through
	arr := []any{"./src/index.ts", 42}
	result := rewriteExportsValue(arr)
	// Should return as-is since it's not a string or map
	if result == nil {
		t.Error("expected non-nil result for array")
	}
}

// ---- PackageNpm ----

func TestPackageNpm_FullPipeline(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	// Create workspace package.json
	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace","license":"MIT"}`), 0644)

	// Create workspace config
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/mylib"]}`), 0644)

	// Create project package.json
	os.MkdirAll(projRoot, 0755)
	pkgJSON := `{"name":"@test/mylib","version":"1.0.0","main":"src/index.ts","exports":{".":"./src/index.ts"},"dependencies":{"react":"^18.0.0"},"devDependencies":{"typescript":"^5.0.0"},"scripts":{"build":"tsc"}}`
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(pkgJSON), 0644)

	// Create build output
	buildOutputDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package")
	libDir := filepath.Join(buildOutputDir, "lib")
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export const x = 1;"), 0644)

	// Create LICENSE
	os.WriteFile(filepath.Join(wsDir, "LICENSE"), []byte("MIT License"), 0644)

	versionInfo := &git.VersionInfo{SHA: "abc1234", Suffix: "abc1234", Branch: "main"}

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, versionInfo, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Version != "1.0.0-abc1234" {
		t.Errorf("version = %q, want %q", result.Version, "1.0.0-abc1234")
	}
	if result.Stable {
		t.Error("expected not stable")
	}
	if result.PackageDir == "" {
		t.Error("expected non-empty package dir")
	}

	// Check output package.json
	outPkg := project.ReadPackageJSONSafe(filepath.Join(result.PackageDir, "package.json"))
	if outPkg == nil {
		t.Fatal("expected output package.json")
	}
	if outPkg.Main != "src/index.js" {
		t.Errorf("main = %q, want 'src/index.js' (rewritten)", outPkg.Main)
	}
	if outPkg.DevDependencies != nil {
		t.Error("devDependencies should be stripped")
	}

	// Check LICENSE was copied
	if !project.FileExists(filepath.Join(result.PackageDir, "LICENSE")) {
		t.Error("expected LICENSE to be copied")
	}
}

func TestPackageNpm_StagedModesDoNotDependOnUmask(t *testing.T) {
	_, modes077 := packageNPMTarballUnderUmask(t, 0o077, false)
	_, modes022 := packageNPMTarballUnderUmask(t, 0o022, false)
	wantModes := map[string]os.FileMode{
		"stage/.":            0o755,
		"stage/index.js":     0o644,
		"stage/cli.js":       0o755,
		"stage/authored.js":  0o755,
		"stage/plain.js":     0o644,
		"stage/package.json": 0o644,
	}
	assertNPMModes(t, modes077, modes022, wantModes)
}

func TestPackageNpm_ArchiveDigestAndModesDoNotDependOnUmask(t *testing.T) {
	if _, err := stdlibexec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed in this Go-only test environment")
	}
	digest077, modes077 := packageNPMTarballUnderUmask(t, 0o077, true)
	digest022, modes022 := packageNPMTarballUnderUmask(t, 0o022, true)
	if digest077 != digest022 {
		t.Fatalf("npm tarball digest depends on umask: 077=%s 022=%s", digest077, digest022)
	}
	wantModes := map[string]os.FileMode{
		"tar/index.js":     0o644,
		"tar/cli.js":       0o755,
		"tar/authored.js":  0o755,
		"tar/plain.js":     0o644,
		"tar/package.json": 0o644,
	}
	assertNPMModes(t, modes077, modes022, wantModes)
}

func assertNPMModes(t *testing.T, modes077, modes022, wantModes map[string]os.FileMode) {
	t.Helper()
	for path, want := range wantModes {
		if got := modes077[path]; got != want {
			t.Errorf("077 mode %s = %#o, want %#o", path, got, want)
		}
		if got := modes022[path]; got != want {
			t.Errorf("022 mode %s = %#o, want %#o", path, got, want)
		}
	}
}

func packageNPMTarballUnderUmask(t *testing.T, mask int, pack bool) (string, map[string]os.FileMode) {
	t.Helper()
	defer setUmask(t, mask)()

	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)
	if err := os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/mylib"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@test/mylib","version":"1.0.0","bin":{"mylib":"./cli.js"}}`
	if err := os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Disk permission bits never decide which files are executable.
	for _, file := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{name: "index.js", content: "export {};\n", mode: 0o644},
		{name: "cli.js", content: "export {};\n", mode: 0o644},                          // package.json bin makes it executable.
		{name: "authored.js", content: "#!/usr/bin/env bun\nexport {};\n", mode: 0o644}, // the "#!" line makes it executable.
		{name: "plain.js", content: "export {};\n", mode: 0o755},                        // an executable bit alone does not.
	} {
		if err := os.WriteFile(filepath.Join(libDir, file.name), []byte(file.content), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	plan := &NpmReleaseSetPlan{
		Coordinate: "@test/mylib",
		Version:    "1.0.0-r42",
		Versions:   map[string]string{"@test/mylib": "1.0.0-r42"},
	}
	versionInfo := &git.VersionInfo{SHA: "r42", Branch: "main"}
	result, err := PackageNpmWithReleaseSet(wsDir, projectPath, "@test/mylib", false, versionInfo, "1.0.0", plan)
	if err != nil {
		t.Fatalf("package under umask %#o: %v", mask, err)
	}

	modes := make(map[string]os.FileMode)
	for _, rel := range []string{".", "index.js", "cli.js", "authored.js", "plain.js", "package.json"} {
		info, err := os.Stat(filepath.Join(result.PackageDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		modes["stage/"+rel] = info.Mode().Perm()
	}
	if !pack {
		return "", modes
	}
	archiveDir := t.TempDir()
	packed, runErr := sdkexec.Run("npm", []string{"pack", ".", "--json", "--pack-destination", archiveDir}, sdkexec.Dir(result.PackageDir))
	if runErr != nil || packed == nil || !packed.Success {
		t.Fatalf("npm pack under umask %#o failed: err=%v stderr=%s", mask, runErr, execStderrForTest(packed))
	}
	matches, err := filepath.Glob(filepath.Join(archiveDir, "*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("npm pack under umask %#o produced %d archives", mask, len(matches))
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rel := strings.TrimPrefix(strings.TrimSuffix(header.Name, "/"), "package/")
		if rel == "package" {
			rel = "."
		}
		modes["tar/"+rel] = os.FileMode(header.Mode & 0o777)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), modes
}

func execStderrForTest(result *sdkexec.Result) string {
	if result == nil {
		return ""
	}
	return result.Stderr
}

func TestPackageNpm_StableRelease(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/mylib"]}`), 0644)

	os.MkdirAll(projRoot, 0755)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"@test/mylib","version":"1.0.0"}`), 0644)

	buildOutputDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package")
	libDir := filepath.Join(buildOutputDir, "lib")
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export const x = 1;"), 0644)

	versionInfo := &git.VersionInfo{SHA: "abc1234", Suffix: "abc1234", Branch: "main"}

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", true, versionInfo, "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Version != "2.0.0" {
		t.Errorf("stable version = %q, want %q", result.Version, "2.0.0")
	}
	if !result.Stable {
		t.Error("expected stable")
	}
}

func TestPackageNpm_NoBuildOutput(t *testing.T) {
	wsDir := t.TempDir()
	result, err := PackageNpm(wsDir, "packages/missing", "missing", false, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result when no build output")
	}
}

func TestPackageNpm_EmptyLibOutput(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0755)

	_, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, nil, "1.0.0")
	if err == nil {
		t.Error("expected error for empty lib output")
	}
}

func TestPackageNpm_WithTypesOutput(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":[]}`), 0644)

	os.MkdirAll(projRoot, 0755)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"@test/mylib","version":"1.0.0"}`), 0644)

	buildOutputDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package")
	libDir := filepath.Join(buildOutputDir, "lib")
	typesDir := filepath.Join(buildOutputDir, "types")
	os.MkdirAll(libDir, 0755)
	os.MkdirAll(typesDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(typesDir, "index.d.ts"), []byte("export declare const x: number;"), 0644)
	os.WriteFile(filepath.Join(typesDir, "package.json"), []byte(`{"types":"./index.d.ts"}`), 0644)

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Check types were merged
	outPkg := project.ReadPackageJSONSafe(filepath.Join(result.PackageDir, "package.json"))
	if outPkg == nil {
		t.Fatal("expected output package.json")
	}
	if outPkg.Types != "./index.d.ts" {
		t.Errorf("types = %q, want './index.d.ts'", outPkg.Types)
	}

	// Check .d.ts file was copied
	if !project.FileExists(filepath.Join(result.PackageDir, "index.d.ts")) {
		t.Error("expected index.d.ts to be copied")
	}
}

func TestPackageNpm_InjectsExportTypes(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":[]}`), 0644)

	os.MkdirAll(projRoot, 0755)
	// "." is a condition object (browser/default); "./hooks" is a bare string
	// whose .d.ts will be emitted; "./gen" is a bare string whose .d.ts is NOT
	// emitted (generated entrypoint, no declaration) and must stay untouched.
	pkgJSON := `{"name":"@test/mylib","version":"1.0.0","exports":{` +
		`".":{"browser":"./src/index.browser.ts","default":"./src/index.ts"},` +
		`"./hooks":"./src/server/hooks.ts",` +
		`"./gen":"./.gen/src/serve.ts"}}`
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(pkgJSON), 0644)

	buildOutputDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package")
	libDir := filepath.Join(buildOutputDir, "lib")
	typesDir := filepath.Join(buildOutputDir, "types")

	// Transpiled JS (structure-preserving under lib/).
	os.MkdirAll(filepath.Join(libDir, "src", "server"), 0755)
	os.MkdirAll(filepath.Join(libDir, ".gen", "src"), 0755)
	os.WriteFile(filepath.Join(libDir, "src", "index.js"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(libDir, "src", "index.browser.js"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(libDir, "src", "server", "hooks.js"), []byte("export const h = 1;"), 0644)
	os.WriteFile(filepath.Join(libDir, ".gen", "src", "serve.js"), []byte("export const s = 1;"), 0644)

	// Emitted declarations: index (both) and hooks get .d.ts; the generated
	// serve entrypoint does NOT (RunTypes never emitted it).
	os.MkdirAll(filepath.Join(typesDir, "src", "server"), 0755)
	os.WriteFile(filepath.Join(typesDir, "src", "index.d.ts"), []byte("export declare const x: number;"), 0644)
	os.WriteFile(filepath.Join(typesDir, "src", "index.browser.d.ts"), []byte("export declare const x: number;"), 0644)
	os.WriteFile(filepath.Join(typesDir, "src", "server", "hooks.d.ts"), []byte("export declare const h: number;"), 0644)

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	pkgPath := filepath.Join(result.PackageDir, "package.json")
	outPkg := project.ReadPackageJSONSafe(pkgPath)
	if outPkg == nil {
		t.Fatal("expected output package.json")
	}

	// Top-level types set from the "." export .d.ts (helps node10 resolution).
	if outPkg.Types != "./src/index.d.ts" {
		t.Errorf("top-level types = %q, want %q", outPkg.Types, "./src/index.d.ts")
	}

	// Parse exports as generic to inspect condition maps.
	var exports map[string]json.RawMessage
	if err := json.Unmarshal(outPkg.Exports, &exports); err != nil {
		t.Fatalf("exports not an object: %v", err)
	}

	// "." (object form) gained a types condition pointing at index.d.ts.
	var dotConds map[string]string
	if err := json.Unmarshal(exports["."], &dotConds); err != nil {
		t.Fatalf("exports['.'] not an object: %v", err)
	}
	if dotConds["types"] != "./src/index.d.ts" {
		t.Errorf("exports['.'].types = %q, want %q", dotConds["types"], "./src/index.d.ts")
	}
	if dotConds["default"] != "./src/index.js" {
		t.Errorf("exports['.'].default = %q, want %q", dotConds["default"], "./src/index.js")
	}

	// "./hooks" (bare string) upgraded to object form with types.
	var hooksConds map[string]string
	if err := json.Unmarshal(exports["./hooks"], &hooksConds); err != nil {
		t.Fatalf("exports['./hooks'] not upgraded to object: %v", err)
	}
	if hooksConds["types"] != "./src/server/hooks.d.ts" {
		t.Errorf("exports['./hooks'].types = %q, want %q", hooksConds["types"], "./src/server/hooks.d.ts")
	}
	if hooksConds["default"] != "./src/server/hooks.js" {
		t.Errorf("exports['./hooks'].default = %q, want %q", hooksConds["default"], "./src/server/hooks.js")
	}

	// "./gen" has no emitted .d.ts: it must stay a bare string, no bogus types.
	var genStr string
	if err := json.Unmarshal(exports["./gen"], &genStr); err != nil {
		t.Fatalf("exports['./gen'] should remain a bare string, got: %s", exports["./gen"])
	}
	if genStr != "./.gen/src/serve.js" {
		t.Errorf("exports['./gen'] = %q, want %q", genStr, "./.gen/src/serve.js")
	}

	// types must serialize FIRST in each condition object (TypeScript requirement).
	rawBytes, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("reading package.json: %v", err)
	}
	assertTypesFirst(t, rawBytes)

	// Determinism: a second run over the same fixture is byte-identical apart
	// from gitInfo.buildTime, which intentionally describes each invocation.
	result2, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error on rerun: %v", err)
	}
	rawBytes2, err := os.ReadFile(filepath.Join(result2.PackageDir, "package.json"))
	if err != nil {
		t.Fatalf("reading package.json rerun: %v", err)
	}
	assertTypesFirst(t, rawBytes2)
	if !bytes.Equal(maskPublishedBuildTime(t, rawBytes), maskPublishedBuildTime(t, rawBytes2)) {
		t.Errorf("published package.json not byte-stable across runs")
	}
}

// maskPublishedBuildTime replaces only the volatile gitInfo.buildTime JSON
// value while preserving every other byte, including key order and spacing.
func maskPublishedBuildTime(t *testing.T, pkgBytes []byte) []byte {
	t.Helper()

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(pkgBytes, &doc); err != nil {
		t.Fatalf("package.json not an object: %v", err)
	}
	var gitInfo map[string]json.RawMessage
	if err := json.Unmarshal(doc["gitInfo"], &gitInfo); err != nil {
		t.Fatalf("package.json gitInfo not an object: %v", err)
	}
	buildTime, ok := gitInfo["buildTime"]
	if !ok {
		t.Fatal("package.json gitInfo.buildTime missing")
	}

	needle := append([]byte(`"buildTime": `), buildTime...)
	if count := bytes.Count(pkgBytes, needle); count != 1 {
		t.Fatalf("package.json gitInfo.buildTime occurrence count = %d, want 1", count)
	}
	return bytes.Replace(pkgBytes, needle, []byte(`"buildTime": "<build-time>"`), 1)
}

// assertTypesFirst verifies that within the exports object, any "types"
// condition key is the FIRST key of its enclosing condition object (i.e.
// preceded only by the opening brace and whitespace). TypeScript requires this
// ordering. The top-level "types" field (a sibling of name/version at document
// depth 1) is intentionally not constrained. Depth 1 is the document root, the
// exports object opens at depth 2, so condition objects live at depth >= 3.
func assertTypesFirst(t *testing.T, pkgBytes []byte) {
	t.Helper()
	var exports map[string]json.RawMessage
	{
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(pkgBytes, &doc); err != nil {
			t.Fatalf("package.json not an object: %v", err)
		}
		if raw, ok := doc["exports"]; ok {
			if err := json.Unmarshal(raw, &exports); err != nil {
				return // exports is a bare string; nothing to check
			}
		}
	}

	for key, rawEntry := range exports {
		// Only condition objects can carry a types key; bare strings can't.
		trimmed := strings.TrimSpace(string(rawEntry))
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		// The first key inside the object: strip the leading brace, then the
		// first quoted token must be the key name.
		inner := strings.TrimSpace(trimmed[1:])
		if !strings.Contains(trimmed, `"types"`) {
			continue // this entry has no types condition — fine
		}
		if !strings.HasPrefix(inner, `"types"`) {
			t.Errorf("exports[%q]: types condition is present but not the first key: %s", key, trimmed)
		}
	}
}

func TestPackageNpm_StableNoWsVersion(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":[]}`), 0644)

	os.MkdirAll(projRoot, 0755)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"@test/mylib","version":"1.0.0"}`), 0644)

	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("x"), 0644)

	_, err := PackageNpm(wsDir, projectPath, "@test/mylib", true, nil, "")
	if err == nil {
		t.Error("expected error for stable release without wsVersion")
	}
}

func TestPackageNpm_WithWorkspaceDeps(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/mylib","packages/utils"]}`), 0644)

	utilsDir := filepath.Join(wsDir, "packages", "utils")
	os.MkdirAll(utilsDir, 0755)
	os.WriteFile(filepath.Join(utilsDir, "package.json"), []byte(`{"name":"@test/utils"}`), 0644)

	os.MkdirAll(projRoot, 0755)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"@test/mylib","version":"1.0.0","dependencies":{"@test/utils":"workspace:^"}}`), 0644)

	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("x"), 0644)

	versionInfo := &git.VersionInfo{SHA: "abc1234", Suffix: "abc1234"}

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, versionInfo, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	outPkg := project.ReadPackageJSONSafe(filepath.Join(result.PackageDir, "package.json"))
	if outPkg == nil {
		t.Fatal("expected output package.json")
	}
	if outPkg.Dependencies["@test/utils"] != "^1.0.0-abc1234" {
		t.Errorf("workspace dep = %q, want %q", outPkg.Dependencies["@test/utils"], "^1.0.0-abc1234")
	}
}

func TestPackageNpmWithReleaseSet_DownstreamOnlyPinsOldUpstreamAcrossPublishedSections(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/downstream"
	projRoot := filepath.Join(wsDir, projectPath)
	os.MkdirAll(projRoot, 0o755)
	os.MkdirAll(filepath.Join(wsDir, "packages", "upstream"), 0o755)
	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{
		"name":"workspace",
		"catalog":{"external":"^9.1.0"},
		"catalogs":{"internal":{"upstream":"workspace:~"}}
	}`), 0o644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/upstream","packages/downstream"]}`), 0o644)
	os.WriteFile(filepath.Join(wsDir, "packages", "upstream", "package.json"), []byte(`{"name":"upstream","version":"1.0.0"}`), 0o644)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{
		"name":"downstream","version":"1.0.0",
		"dependencies":{"upstream":"workspace:*","external":"catalog:"},
		"peerDependencies":{"upstream":"workspace:^"},
		"optionalDependencies":{"upstream":"catalog:internal"},
		"devDependencies":{"upstream":"workspace:*"}
	}`), 0o644)
	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0o755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export {};"), 0o644)

	result, err := PackageNpmWithReleaseSet(wsDir, projectPath, "downstream", false, nil, "9.9.9", &NpmReleaseSetPlan{
		Coordinate: "downstream",
		Version:    "1.0.0-r42",
		Versions: map[string]string{
			"downstream": "1.0.0-r42",
			"upstream":   "1.0.0-r41",
		},
	})
	if err != nil {
		t.Fatalf("PackageNpmWithReleaseSet: %v", err)
	}
	out := project.ReadPackageJSONSafe(filepath.Join(result.PackageDir, "package.json"))
	if result.Version != "1.0.0-r42" || out.Version != "1.0.0-r42" {
		t.Fatalf("downstream version = %q/%q, want R42", result.Version, out.Version)
	}
	if got := out.Dependencies["upstream"]; got != "1.0.0-r41" {
		t.Errorf("workspace:* upstream = %q, want exact old R41", got)
	}
	if got := out.PeerDependencies["upstream"]; got != "^1.0.0-r41" {
		t.Errorf("workspace:^ upstream = %q, want ^old R41", got)
	}
	if got := out.OptionalDependencies["upstream"]; got != "~1.0.0-r41" {
		t.Errorf("catalog internal upstream = %q, want ~old R41", got)
	}
	if got := out.Dependencies["external"]; got != "^9.1.0" {
		t.Errorf("external catalog entry = %q, want concrete authored catalog value", got)
	}
	if out.DevDependencies != nil {
		t.Error("devDependencies must remain excluded from the published package")
	}
	firstPackageJSON, err := os.ReadFile(filepath.Join(result.PackageDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(firstPackageJSON, []byte(`"buildTime"`)) {
		t.Fatal("sparse immutable package contains invocation time")
	}
	second, err := PackageNpmWithReleaseSet(wsDir, projectPath, "downstream", false, nil, "9.9.9", &NpmReleaseSetPlan{
		Coordinate: "downstream", Version: "1.0.0-r42",
		Versions: map[string]string{"downstream": "1.0.0-r42", "upstream": "1.0.0-r41"},
	})
	if err != nil {
		t.Fatalf("PackageNpmWithReleaseSet retry: %v", err)
	}
	secondPackageJSON, err := os.ReadFile(filepath.Join(second.PackageDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPackageJSON, secondPackageJSON) {
		t.Fatal("retrying one sparse plan changed immutable package.json bytes")
	}
	if err := os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"wrong-coordinate","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackageNpmWithReleaseSet(wsDir, projectPath, "downstream", false, nil, "9.9.9", &NpmReleaseSetPlan{
		Coordinate: "downstream", Version: "1.0.0-r42", Versions: map[string]string{"downstream": "1.0.0-r42"},
	}); err == nil || !strings.Contains(err.Error(), "does not match release-set coordinate") {
		t.Fatalf("staged coordinate mismatch error = %v", err)
	}
	if project.FileExists(filepath.Join(wsDir, ".putnami", "out", "packages/upstream", "package", "npm")) {
		t.Error("unchanged upstream was unexpectedly packaged")
	}
}

func TestPackageNpmWithReleaseSet_UpstreamChangeRepackagesDownstreamAgainstCandidate(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/downstream"
	projRoot := filepath.Join(wsDir, projectPath)
	os.MkdirAll(projRoot, 0o755)
	os.MkdirAll(filepath.Join(wsDir, "packages", "upstream"), 0o755)
	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0o644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/upstream","packages/downstream"]}`), 0o644)
	os.WriteFile(filepath.Join(wsDir, "packages", "upstream", "package.json"), []byte(`{"name":"upstream"}`), 0o644)
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(`{"name":"downstream","version":"1.0.0","dependencies":{"upstream":"workspace:*"}}`), 0o644)
	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0o755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export {};"), 0o644)

	result, err := PackageNpmWithReleaseSet(wsDir, projectPath, "downstream", false, nil, "1.0.0", &NpmReleaseSetPlan{
		Coordinate: "downstream",
		Version:    "1.0.0-r43",
		Versions: map[string]string{
			"downstream": "1.0.0-r43",
			"upstream":   "1.0.0-r43",
		},
	})
	if err != nil {
		t.Fatalf("PackageNpmWithReleaseSet: %v", err)
	}
	out := project.ReadPackageJSONSafe(filepath.Join(result.PackageDir, "package.json"))
	if got := out.Dependencies["upstream"]; got != "1.0.0-r43" {
		t.Fatalf("repackaged downstream upstream version = %q, want candidate R43", got)
	}
}

func TestPackageNpmWithReleaseSet_RejectsMissingOrUnresolvedInternalMembers(t *testing.T) {
	if _, err := PackageNpmWithReleaseSet("", "", "downstream", false, nil, "", &NpmReleaseSetPlan{
		Coordinate: "downstream", Version: "1.0.0-r42", Versions: map[string]string{"upstream": "1.0.0-r41"},
	}); err == nil || !strings.Contains(err.Error(), "missing the selected member") {
		t.Fatalf("missing selected member error = %v", err)
	}
	_, err := resolveSparseDependencies(
		map[string]string{"private-workspace": "workspace:*"},
		map[string]bool{"private-workspace": true}, nil,
		map[string]string{"downstream": "1.0.0-r42"},
	)
	if err == nil || !strings.Contains(err.Error(), "missing from") {
		t.Fatalf("unresolved internal selector error = %v", err)
	}
}

func TestPackageNpm_StripsOverridesAndResolutions(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/mylib"
	projRoot := filepath.Join(wsDir, projectPath)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"name":"workspace"}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "putnami.workspace.json"), []byte(`{"projects":["packages/mylib"]}`), 0644)

	os.MkdirAll(projRoot, 0755)
	pkgJSON := `{"name":"@test/mylib","version":"1.0.0","dependencies":{"@putnami/application":"latest"},"overrides":{"@putnami/application":"file:../putnami/app"},"resolutions":{"@putnami/application":"file:../putnami/app"}}`
	os.WriteFile(filepath.Join(projRoot, "package.json"), []byte(pkgJSON), 0644)

	libDir := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "lib")
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(libDir, "index.js"), []byte("export const x = 1;"), 0644)

	result, err := PackageNpm(wsDir, projectPath, "@test/mylib", false, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Read the raw output package.json to check overrides/resolutions are stripped
	outData, err := os.ReadFile(filepath.Join(result.PackageDir, "package.json"))
	if err != nil {
		t.Fatalf("reading output package.json: %v", err)
	}
	var outRaw map[string]json.RawMessage
	if err := json.Unmarshal(outData, &outRaw); err != nil {
		t.Fatalf("parsing output package.json: %v", err)
	}
	if _, ok := outRaw["overrides"]; ok {
		t.Error("overrides should be stripped from published package.json")
	}
	if _, ok := outRaw["resolutions"]; ok {
		t.Error("resolutions should be stripped from published package.json")
	}
	// dependencies should still be present
	if _, ok := outRaw["dependencies"]; !ok {
		t.Error("dependencies should be preserved")
	}
}

// ---- PackageDocker ----

func TestPackageDocker_DryRun(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/myapp"

	compileOutput := filepath.Join(wsDir, ".putnami", "out", projectPath, "build", "compile")
	os.MkdirAll(compileOutput, 0755)
	os.WriteFile(filepath.Join(compileOutput, "myapp-linux-x64"), []byte("binary"), 0755)

	params := DockerParams{
		Tag:      "v1",
		Platform: "linux/amd64",
		Port:     8080,
		DryRun:   true,
	}

	result, err := PackageDocker(wsDir, "myapp", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result["dryRun"] != true {
		t.Error("expected dryRun=true")
	}
	// The package candidate is content-addressed but deliberately registry-free.
	contentHash, _ := result["contentHash"].(string)
	if len(contentHash) != 12 {
		t.Errorf("expected 12-char contentHash, got %q", result["contentHash"])
	}
	if result["image"] != "myapp:c-"+contentHash {
		t.Errorf("image = %q, want content-addressed tag c-%s", result["image"], contentHash)
	}
	// Dry run must not assemble anything.
	if project.FileExists(filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "docker", "oci")) {
		t.Error("expected no OCI layout to be written on dry run")
	}
}

func TestPackageDocker_DryRunDashifiesMultiSegmentName(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "surfaces/admin/workloads/console"

	compileOutput := filepath.Join(wsDir, ".putnami", "out", projectPath, "build", "compile")
	os.MkdirAll(compileOutput, 0755)
	os.WriteFile(filepath.Join(compileOutput, "console-linux-x64"), []byte("binary"), 0755)

	params := DockerParams{
		Platform: "linux/amd64",
		Port:     8080,
		DryRun:   true,
	}

	// A multi-segment project name flattens to one local candidate component.
	// Publish qualifies it only after resolving the target registry.
	result, err := PackageDocker(wsDir, "surfaces/admin/workloads/console", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	contentHash, _ := result["contentHash"].(string)
	want := "surfaces-admin-workloads-console:c-" + contentHash
	if result["image"] != want {
		t.Errorf("image = %q, want dashified %q", result["image"], want)
	}
}

func TestPackageDocker_DryRunDefaultTag(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/myapp"

	compileOutput := filepath.Join(wsDir, ".putnami", "out", projectPath, "build", "compile")
	os.MkdirAll(compileOutput, 0755)
	os.WriteFile(filepath.Join(compileOutput, "myapp-linux-x64"), []byte("binary"), 0755)

	params := DockerParams{DryRun: true}

	result, err := PackageDocker(wsDir, "myapp", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Without --docker-tag the only ref is the content-addressed tag — there
	// is no mutable local "latest" ref anymore.
	image, _ := result["image"].(string)
	if !strings.HasPrefix(image, "myapp:c-") {
		t.Errorf("image = %q, want content-addressed myapp:c-<hash>", image)
	}
}

func TestPackageDocker_DryRunWithAssets(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/myapp"

	compileOutput := filepath.Join(wsDir, ".putnami", "out", projectPath, "build", "compile")
	os.MkdirAll(compileOutput, 0755)
	os.WriteFile(filepath.Join(compileOutput, "myapp-linux-x64"), []byte("binary"), 0755)

	// Create .gen/public assets
	genPublicDir := filepath.Join(wsDir, projectPath, ".gen", "public")
	os.MkdirAll(genPublicDir, 0755)
	os.WriteFile(filepath.Join(genPublicDir, "style.css"), []byte("body{}"), 0644)

	params := DockerParams{DryRun: true, Port: 3000}

	result, err := PackageDocker(wsDir, "myapp", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify assets were copied to compile output
	if !project.FileExists(filepath.Join(compileOutput, ".gen", "public", "style.css")) {
		t.Error("expected .gen/public assets to be copied to compile output")
	}

	// Asset content participates in the image identity: a different asset
	// byte must produce a different content tag.
	hashWithAssets, _ := result["contentHash"].(string)
	if len(hashWithAssets) != 12 {
		t.Fatalf("expected contentHash in result, got %v", result["contentHash"])
	}
	os.WriteFile(filepath.Join(wsDir, projectPath, ".gen", "public", "style.css"), []byte("body{color:red}"), 0644)
	changed, err := PackageDocker(wsDir, "myapp", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed["contentHash"] == hashWithAssets {
		t.Error("expected content hash to change when assets change")
	}
}

func TestPackageDocker_NoCompileOutput(t *testing.T) {
	wsDir := t.TempDir()
	_, err := PackageDocker(wsDir, "myapp", "packages/missing", DockerParams{}, nil, "1.0.0")
	if err == nil {
		t.Error("expected error when no compile output")
	}
}

func TestPackageDocker_FallbackCompileOutput(t *testing.T) {
	wsDir := t.TempDir()
	projectPath := "packages/myapp"

	// Use package/ path instead of build/
	compileOutput := filepath.Join(wsDir, ".putnami", "out", projectPath, "package", "compile")
	os.MkdirAll(compileOutput, 0755)
	os.WriteFile(filepath.Join(compileOutput, "myapp-linux-x64"), []byte("binary"), 0755)

	params := DockerParams{DryRun: true}

	result, err := PackageDocker(wsDir, "myapp", projectPath, params, nil, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result from fallback path")
	}
}
