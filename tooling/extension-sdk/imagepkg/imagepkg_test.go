package imagepkg

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
)

func TestResolveBaseReferenceUsesOnlyDependencyClosureArtifact(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "local-image-publication-boundary", "project-bases-consume-local-candidates")
	root := t.TempDir()
	basePath := "images/runtime"
	baseImage, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(pkgmeta.PackageOutputDir(root, basePath, "docker"), "oci")
	digest, err := oci.WriteLayout(layout, baseImage)
	if err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(pkgmeta.PackageOutputDir(root, basePath, "docker"), "manifest.json"), pkgmeta.DockerManifest{
		Image: "runtime", Digest: digest, Layout: "oci", Platform: "linux/amd64",
	})
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "service", Path: "apps/service", DependencyClosure: []pctx.ProjectRef{
			{ID: "/apps/service", Name: "service", Path: "apps/service"},
			{ID: "/images/runtime", Name: "runtime", Path: basePath},
		},
	}}

	got, err := ResolveBaseArtifact(ctx, "", "/images/runtime", "linux/amd64")
	if err != nil {
		t.Fatalf("ResolveBaseReference() error = %v", err)
	}
	if got.Reference != "putnami.local/runtime@"+digest || got.Digest != digest || got.Layout != layout {
		t.Fatalf("ResolveBaseArtifact() = %+v, want typed local candidate", got)
	}
	if _, err := ResolveBaseReference(ctx, "", "/images/outside-closure", "linux/amd64"); err == nil {
		t.Fatal("ResolveBaseReference() accepted a project outside the dependency closure")
	}
	if _, err := ResolveBaseReference(ctx, "busybox@"+digest, "/images/runtime", "linux/amd64"); err == nil {
		t.Fatal("ResolveBaseReference() accepted both literal and project bases")
	}
	if _, err := ResolveBaseReference(ctx, "", "/images/runtime", "linux/arm64"); err == nil {
		t.Fatal("ResolveBaseReference() accepted the wrong platform")
	}
}

func TestPackageConsumesProjectBaseLocallyWithoutRegistryCredentials(t *testing.T) {
	baseImage, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	baseDigest, err := baseImage.Digest()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	basePath := "images/private-base"
	if _, err := oci.WriteLayout(filepath.Join(pkgmeta.PackageOutputDir(root, basePath, "docker"), "oci"), baseImage); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(pkgmeta.PackageOutputDir(root, basePath, "docker"), "manifest.json"), pkgmeta.DockerManifest{
		Image: "private-base", Digest: baseDigest.String(), Layout: "oci", Platform: "linux/amd64",
	})
	projectPath := "images/child"
	projectRoot := filepath.Join(root, projectPath)
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "entrypoint"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Layers: []LayerConfig{{Files: []FileConfig{{Source: "entrypoint", Path: "/entrypoint", Mode: 0o755}}}}}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{
			Name: "images/child", Path: projectPath, FullPath: projectRoot, Type: "image",
			DependencyClosure: []pctx.ProjectRef{{ID: "/images/private-base", Name: "private-base", Path: basePath}},
		},
		Params: pctx.Params{
			"image":             mustRaw(t, cfg),
			"dockerBaseProject": mustRaw(t, "/images/private-base"),
		},
	}

	oldResolveToken := registrycred.ResolveToken
	registrycred.ResolveToken = func(host string) (string, string) {
		t.Fatalf("local project base unexpectedly resolved registry credentials for %q", host)
		return "", ""
	}
	defer func() { registrycred.ResolveToken = oldResolveToken }()

	status, _, err := Package(ctx, jsonl.New())
	if err != nil || status != "OK" {
		t.Fatalf("Package() = (%q, %v), want local project base assembly to succeed", status, err)
	}
}

func TestPackageAlwaysProducesLocalCandidateWithoutOutputRegistryRequests(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "local-image-publication-boundary", "package-owns-only-local-candidate-evidence")
	// This test exercises the native/cloudless registry floor. A parent Putnami
	// test run advertises its own CLI to the extension process; do not turn this
	// local-registry fixture into a real cloud credential callback.
	t.Setenv(registryproto.CLIExecutableEnv, "")
	baseRegistry := httptest.NewServer(registry.New())
	defer baseRegistry.Close()
	var outputRequests atomic.Int64
	outputRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		outputRequests.Add(1)
		http.Error(w, "package must not contact output registry", http.StatusInternalServerError)
	}))
	defer outputRegistry.Close()
	outputHost := strings.TrimPrefix(outputRegistry.URL, "http://")
	t.Setenv("DOCKER_REGISTRY", outputHost)
	oldResolveToken := registrycred.ResolveToken
	var outputCredentialRequests atomic.Int64
	registrycred.ResolveToken = func(host string) (string, string) {
		if host == outputHost {
			outputCredentialRequests.Add(1)
			return "", ""
		}
		return oldResolveToken(host)
	}
	defer func() { registrycred.ResolveToken = oldResolveToken }()

	baseHost := strings.TrimPrefix(baseRegistry.URL, "http://")
	baseTag, err := name.NewTag(baseHost+"/base:seed", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	baseImage, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(baseTag, baseImage); err != nil {
		t.Fatal(err)
	}
	baseDigest, err := baseImage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	baseRef := baseTag.Context().Digest(baseDigest.String()).String()

	root := t.TempDir()
	projectPath := "images/toolchain"
	projectRoot := filepath.Join(root, projectPath)
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "entrypoint"), []byte("#!/bin/sh\nexec /tool/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Base:     baseRef,
		Registry: outputHost,
		Layers:   []LayerConfig{{Files: []FileConfig{{Source: "entrypoint", Path: "/entrypoint", Mode: 0o755}}}},
		Env:      []string{"TOOL_MODE=hermetic"}, Entrypoint: []string{"/entrypoint"},
	}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "images/toolchain", Path: projectPath, FullPath: projectRoot, Type: "image"},
		Params: pctx.Params{
			"image":          mustRaw(t, cfg),
			"dockerRegistry": mustRaw(t, strings.TrimPrefix(outputRegistry.URL, "http://")),
		},
	}

	status, first, err := Package(ctx, jsonl.New())
	if err != nil || status != "OK" {
		t.Fatalf("first Package() = (%q, %+v, %v), want OK", status, first, err)
	}
	hash, _ := first["contentHash"].(string)
	if len(hash) != 64 || first["version"] != "c-"+hash || first["candidateDigest"] == "" {
		t.Fatalf("package identity = %+v, want complete local candidate identity", first)
	}
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if _, err := os.Stat(filepath.Join(dockerDir, "oci", "index.json")); err != nil {
		t.Fatalf("miss did not assemble OCI layout: %v", err)
	}

	manifest, err := pkgmeta.ReadDockerManifest(root, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Layout != "oci" || manifest.Version != "c-"+hash || manifest.Digest != first["candidateDigest"] {
		t.Fatalf("local candidate manifest = %+v", manifest)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(dockerDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestBytes), "immutableRef") || strings.Contains(string(manifestBytes), "published") {
		t.Fatalf("package manifest contains publication evidence: %s", manifestBytes)
	}
	if outputRequests.Load() != 0 {
		t.Fatalf("package made %d output registry requests", outputRequests.Load())
	}
	if outputCredentialRequests.Load() != 0 {
		t.Fatalf("package made %d output registry credential requests", outputCredentialRequests.Load())
	}
}

func TestResolveSpecHashIncludesFileBytesAndRuntimeConfig(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "tool")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Layers: []LayerConfig{{Files: []FileConfig{{Source: "tool", Path: "/tool", Mode: 0o755}}}}, Env: []string{"A=1"}}
	spec, err := resolveSpec(root, "example.com/base@sha256:"+strings.Repeat("b", 64), "linux/amd64", cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := oci.ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := oci.ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("file byte change did not change image content hash")
	}
	spec.Env = []string{"A=2"}
	third, err := oci.ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatal("runtime config change did not change image content hash")
	}
}

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestJSON(t *testing.T, filename string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
