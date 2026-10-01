package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// TestEnsureArtifact_WarmPathSkipsInstall: when the stable symlink already
// resolves to a manifest, ensure is a stat — it must not contact the registry.
func TestEnsureArtifact_WarmPathSkipsInstall(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(t.TempDir(), "art")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "putnami.extension.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifactGlobal(ws, layout.Extensions, "@putnami/go", target); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("warm ensure must not contact the registry")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: ws, ResolverURL: srv.URL, HTTPClient: srv.Client()}
	if err := inst.EnsureExtension(context.Background(), "@putnami/go", "latest", nil); err != nil {
		t.Fatalf("warm ensure: %v", err)
	}
}

// TestEnsureArtifact_HealsFromStore: a fresh worktree whose link is absent but
// whose lock pins a digest already in the shared store links it with no download.
func TestEnsureArtifact_HealsFromStore(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir()) // shared store for both installers

	archive := buildExtensionArchive(t, "@putnami/test", "1.0.0")
	digest := sha256Bytes(archive)
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.0.0")))

	// Warm the shared store via a verified install in another worktree.
	warmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Header().Set("X-Integrity", "sha256:"+digest)
		w.Write(archive)
	}))
	defer warmSrv.Close()
	warm := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: warmSrv.URL, HTTPClient: warmSrv.Client()}
	if _, err := warm.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("warm install: %v", err)
	}

	// Fresh worktree: ensure must link from the store, never download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("ensure must not download when the store already has the digest")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ws := t.TempDir()
	inst := &Installer{WorkspaceRoot: ws, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lock := &lockfile.LockEntry{
		Version:      "1.0.0",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{currentPlatform(): digest},
	}
	if err := inst.EnsureExtension(context.Background(), "@putnami/test", "latest", lock); err != nil {
		t.Fatalf("ensure heal: %v", err)
	}
	if !manifestResolves(layout.StableDir(ws, layout.Extensions, "@putnami/test"), "putnami.extension.json") {
		t.Error("ensure should have linked the extension into the worktree")
	}
}

// TestEnsureArtifact_MigratesLegacyToStore: a worktree with a legacy
// per-worktree install migrates to the shared store (relink + drop the legacy
// tree) once a sibling has warmed it, with no download.
func TestEnsureArtifact_MigratesLegacyToStore(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	archive := buildExtensionArchive(t, "@putnami/test", "1.0.0")
	digest := sha256Bytes(archive)
	manifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "1.0.0")))

	// A sibling worktree warms the shared store.
	warmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Header().Set("X-Integrity", "sha256:"+digest)
		w.Write(archive)
	}))
	defer warmSrv.Close()
	warm := &Installer{WorkspaceRoot: t.TempDir(), ResolverURL: warmSrv.URL, HTTPClient: warmSrv.Client()}
	if _, err := warm.Install(context.Background(), "@putnami/test", "latest", nil); err != nil {
		t.Fatalf("warm install: %v", err)
	}

	// This worktree has a legacy per-worktree install (relative symlink → ArtifactDir).
	ws := t.TempDir()
	legacy := layout.ArtifactDir(ws, layout.Extensions, "@putnami/test", "1.0.0")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "putnami.extension.json"),
		[]byte(extensionManifestJSON("@putnami/test", "1.0.0")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifact(ws, layout.Extensions, "@putnami/test", "1.0.0"); err != nil {
		t.Fatal(err)
	}

	// Ensure must migrate to the shared store — without a download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("migration must not download when the store already has the digest")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	inst := &Installer{WorkspaceRoot: ws, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	lock := &lockfile.LockEntry{
		Version:      "1.0.0",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{currentPlatform(): digest},
	}
	if err := inst.EnsureExtension(context.Background(), "@putnami/test", "latest", lock); err != nil {
		t.Fatalf("ensure migrate: %v", err)
	}

	// The symlink now points into the shared store...
	target, err := os.Readlink(layout.StableDir(ws, layout.Extensions, "@putnami/test"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if !strings.Contains(target, "sha256") {
		t.Errorf("symlink should point into the shared store, got %q", target)
	}
	// ...and the legacy per-worktree tree is gone.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy per-worktree tree should be removed after migration")
	}
}

func TestEnsureArtifact_ReinstallsWhenManifestDoesNotMatchLock(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	ws := t.TempDir()
	oldTarget := filepath.Join(t.TempDir(), "old")
	if err := os.MkdirAll(oldTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldTarget, "putnami.extension.json"),
		[]byte(extensionManifestJSON("@putnami/test", "1.0.0")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifactGlobal(ws, layout.Extensions, "@putnami/test", oldTarget); err != nil {
		t.Fatal(err)
	}

	newArchive := buildExtensionArchive(t, "@putnami/test", "2.0.0")
	newDigest := sha256Bytes(newArchive)
	newManifestHash := sha256Bytes([]byte(extensionManifestJSON("@putnami/test", "2.0.0")))
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-Resolved-Version", "2.0.0")
		w.Write(newArchive)
	}))
	defer srv.Close()

	inst := &Installer{WorkspaceRoot: ws, ResolverURL: srv.URL, HTTPClient: srv.Client()}
	lock := &lockfile.LockEntry{
		Version:      "2.0.0",
		ManifestHash: newManifestHash,
		Integrities:  map[string]string{currentPlatform(): newDigest},
	}
	if err := inst.EnsureExtension(context.Background(), "@putnami/test", "latest", lock); err != nil {
		t.Fatalf("ensure should reinstall stale artifact: %v", err)
	}
	if hits == 0 {
		t.Fatal("ensure should download when the existing manifest does not match the lock")
	}
	got, err := HashFile(filepath.Join(layout.StableDir(ws, layout.Extensions, "@putnami/test"), "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got != newManifestHash {
		t.Errorf("stable link still points at stale manifest: got %s, want %s", got, newManifestHash)
	}
}

// TestManifestResolves covers the resolvable / missing / dangling cases.
func TestManifestResolves(t *testing.T) {
	ws := t.TempDir()
	stable := layout.StableDir(ws, layout.Extensions, "@putnami/x")

	// Missing link.
	if manifestResolves(stable, "putnami.extension.json") {
		t.Error("missing link should not resolve")
	}

	// Resolvable link.
	target := filepath.Join(t.TempDir(), "art")
	_ = os.MkdirAll(target, 0o755)
	_ = os.WriteFile(filepath.Join(target, "putnami.extension.json"), []byte(`{}`), 0o644)
	_ = layout.LinkArtifactGlobal(ws, layout.Extensions, "@putnami/x", target)
	if !manifestResolves(stable, "putnami.extension.json") {
		t.Error("linked manifest should resolve")
	}

	// Dangling link (target removed) must report unresolved so ensure heals it.
	_ = os.RemoveAll(target)
	if manifestResolves(stable, "putnami.extension.json") {
		t.Error("dangling link should not resolve")
	}
}

// TestEnsureArtifact_UnlinksDanglingSymlinkOnHealFailure: when the stable link is
// dangling (its shared-store target was reaped, e.g. by a concurrent
// `cache clean --all`) and the heal cannot re-download it (a non-redownloadable
// extension like @putnami/cloud), ensure must drop the dangling link so the
// extension reads as cleanly absent instead of fork/exec'ing a missing binary.
func TestEnsureArtifact_UnlinksDanglingSymlinkOnHealFailure(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	ws := t.TempDir()
	// Link to a target, then reap the target → the exact dangling-link state a
	// GC'd / cleaned CAS entry leaves behind.
	target := filepath.Join(t.TempDir(), "reaped")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "putnami.extension.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifactGlobal(ws, layout.Extensions, "@putnami/cloud", target); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(target)

	stable := layout.StableDir(ws, layout.Extensions, "@putnami/cloud")
	if manifestResolves(stable, "putnami.extension.json") {
		t.Fatal("precondition: link should be dangling")
	}

	// The registry cannot serve this extension → the heal download fails.
	srv := recorded.NewServer(t, nil, putRegistryRecording(t, "download-version-not-found.404.http"))

	inst := &Installer{WorkspaceRoot: ws, ResolverURL: srv.URL, HTTPClient: srv.Client()}
	lock := &lockfile.LockEntry{
		Version:     "1.0.0",
		Integrities: map[string]string{currentPlatform(): sha256Bytes([]byte("not-in-store"))},
	}
	if err := inst.EnsureExtension(context.Background(), "@putnami/cloud", "latest", lock); err == nil {
		t.Fatal("ensure should fail when a dangling extension cannot be re-downloaded")
	}

	// The dangling link must be gone — not left to fork/exec a missing binary.
	if _, err := os.Lstat(stable); !os.IsNotExist(err) {
		t.Errorf("dangling stable symlink should be unlinked after heal failure; Lstat err = %v", err)
	}
}
