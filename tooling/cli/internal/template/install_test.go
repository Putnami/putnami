package template

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Template installation fixtures use local HTTP servers. Their credential seam
// must not launch a developer's real CLI or write into the temporary test store,
// and a hosted run's invocation broker must not replace the fixture's registry
// (the broker wins over every authored route and answers 401 to a test archive).
func TestMain(m *testing.M) {
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	os.Exit(m.Run())
}

// buildTemplateArchive produces a tar.gz containing a minimal valid
// putnami.template.json so Install can pass post-extract validation.
func buildTemplateArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest := `{"name":"` + name + `","version":"` + version + `","description":"test template"}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     ManifestFilename,
		Mode:     0o644,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hashOf(t *testing.T, data []byte) string {
	t.Helper()
	tmp, err := os.CreateTemp("", "tpl-hash-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	h, err := extension.HashFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestInstall_SuccessWithResolvedVersion drives the full template
// installer over a fake registry that returns X-Resolved-Version; the
// archive is extracted, the manifest is verified, and the artifact is
// installed at the version-qualified directory.
func TestInstall_SuccessWithResolvedVersion(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	archive := buildTemplateArchive(t, "@putnami/test-tpl", "1.0.0")
	expectedHash := hashOf(t, archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Header().Set("X-Integrity", "sha256:"+expectedHash)
		w.Write(archive)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}
	result, err := inst.Install(context.Background(), "@putnami/test-tpl", "latest", nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", result.Version, "1.0.0")
	}
	if result.ManifestHash == "" {
		t.Error("ManifestHash should be populated")
	}
	if result.Integrity != expectedHash {
		t.Errorf("Integrity = %q, want hash of archive", result.Integrity)
	}
	if _, err := os.Stat(filepath.Join(result.InstallDir, ManifestFilename)); err != nil {
		t.Errorf("extracted manifest missing: %v", err)
	}
}

// TestInstall_LockfileIntegrityMismatch exercises the lockfile-pin path:
// when a lockfile entry pins a hash but the downloaded archive's hash
// differs, the installer must refuse and surface the mismatch.
func TestInstall_LockfileIntegrityMismatch(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	archive := buildTemplateArchive(t, "@putnami/test-tpl", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Write(archive)
	}))
	defer srv.Close()

	inst := &Installer{
		WorkspaceRoot: t.TempDir(),
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}
	lockEntry := &lockfile.LockEntry{
		Version:   "1.0.0",
		Integrity: strings.Repeat("a", 64), // wrong hash
	}
	_, err := inst.Install(context.Background(), "@putnami/test-tpl", "latest", lockEntry)
	if err == nil {
		t.Fatal("expected integrity mismatch error")
	}
	if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("error should mention integrity mismatch, got: %v", err)
	}
}

// TestInstall_RelocatesFromZeroVersion covers the resolvedVersion=="0.0.0"
// branch in Install: when the resolver does not advertise a version, the
// installer falls back to reading the manifest and relocates the artifact
// to the correct version-qualified directory.
func TestInstall_RelocatesFromZeroVersion(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	// Archive's manifest says version 2.5.0; resolver advertises no version
	// header, so the installer must fall back to the manifest's version.
	archive := buildTemplateArchive(t, "@putnami/relocator", "2.5.0")
	expectedHash := hashOf(t, archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No X-Resolved-Version header.
		w.Header().Set("X-Integrity", "sha256:"+expectedHash)
		w.Write(archive)
	}))
	defer srv.Close()

	wsRoot := t.TempDir()
	inst := &Installer{
		WorkspaceRoot: wsRoot,
		ResolverURL:   srv.URL,
		HTTPClient:    srv.Client(),
	}
	// Constraint is non-parseable so resolvedVersion falls through to 0.0.0,
	// then the installer must relocate to 2.5.0 based on the manifest.
	result, err := inst.Install(context.Background(), "@putnami/relocator", "main", nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Version != "2.5.0" {
		t.Errorf("Version = %q, want %q (resolved from manifest)", result.Version, "2.5.0")
	}
	// The digest-keyed shared store has no version-named dir to relocate: the
	// version is resolved only for the lock, and the manifest lives at the digest
	// dir root.
	if _, err := os.Stat(filepath.Join(result.InstallDir, ManifestFilename)); err != nil {
		t.Errorf("manifest should exist at the resolved install dir: %v", err)
	}
}

// TestInstall_IdempotentCachedInstall verifies the global-store fast path: once
// the shared store holds the digest the lock pins, a fresh worktree installs
// with FromCache=true and no download.
func TestInstall_IdempotentCachedInstall(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir()) // one shared store for both installs

	archive := buildTemplateArchive(t, "@putnami/cached", "1.0.0")
	digest := hashOf(t, archive)

	// First install warms the shared store (advertised integrity → verified).
	warmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Header().Set("X-Integrity", "sha256:"+digest)
		w.Write(archive)
	}))
	defer warmSrv.Close()
	warm := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: warmSrv.URL, HTTPClient: warmSrv.Client()}
	warmResult, err := warm.Install(context.Background(), "@putnami/cached", "latest", nil)
	if err != nil {
		t.Fatalf("warm install: %v", err)
	}

	// A fresh worktree whose lock pins the digest must not hit the network.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("cached install must not hit the network")
	}))
	defer srv.Close()
	inst := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lockEntry := &lockfile.LockEntry{
		Version:      "1.0.0",
		ManifestHash: warmResult.ManifestHash,
		Integrities:  warmResult.Integrities, // pins the digest under linux/x64
	}
	result, err := inst.Install(context.Background(), "@putnami/cached", "latest", lockEntry)
	if err != nil {
		t.Fatalf("Install (cached): %v", err)
	}
	if !result.FromCache {
		t.Error("FromCache should be true when the shared store already has the digest")
	}
}

// TestInstaller_RemoveAndInstalledVersions covers the simple wrappers.
func TestInstaller_RemoveAndInstalledVersions(t *testing.T) {
	wsRoot := t.TempDir()
	inst := &Installer{WorkspaceRoot: wsRoot}

	// Layout two versions on disk and link the latest.
	for _, v := range []string{"1.0.0", "1.1.0"} {
		dir := layout.ArtifactDir(wsRoot, layout.Templates, "@putnami/tpl", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ManifestFilename), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := layout.LinkArtifact(wsRoot, layout.Templates, "@putnami/tpl", "1.1.0"); err != nil {
		t.Fatal(err)
	}

	versions, err := inst.InstalledVersions("@putnami/tpl")
	if err != nil {
		t.Fatalf("InstalledVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("expected 2 installed versions, got %d: %v", len(versions), versions)
	}

	// Remove now unlinks only: the stable symlink is removed, but the on-disk
	// version dirs survive (reclaiming shared bytes is the artifact GC's job).
	if err := inst.Remove("@putnami/tpl", "1.0.0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(layout.StableDir(wsRoot, layout.Templates, "@putnami/tpl")); !os.IsNotExist(err) {
		t.Error("Remove should unlink the stable symlink")
	}
	versions, _ = inst.InstalledVersions("@putnami/tpl")
	if len(versions) != 2 {
		t.Errorf("Remove unlinks only; both version dirs should remain, got %v", versions)
	}
}

// TestInstallerPlatform_IsTheFixedTemplatePlatform pins the template half of the
// artifactOps platform seam: templates are platform-independent, so
// every host records the same key and a version bump has no foreign platform to
// re-resolve.
func TestInstallerPlatform_IsTheFixedTemplatePlatform(t *testing.T) {
	inst := NewInstaller(t.TempDir())
	if got, want := inst.Platform(), lockfile.PlatformKey("linux", "x64"); got != want {
		t.Errorf("Platform() = %q, want %q", got, want)
	}
}
