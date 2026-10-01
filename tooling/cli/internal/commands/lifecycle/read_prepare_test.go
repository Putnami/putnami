package lifecycle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func TestPrepareReadOnlyColdWarmAndNewWorktreePreserveOwnedFiles(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "cold-and-warm-reads-use-the-exact-lock-without-full-install")
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "automatic-reads-never-invoke-cloud-credential-minting")
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "human-and-agent-artifact-ownership-files-are-preserved")
	const (
		name     = "@putnami/cloud"
		version  = "0.1.0-locked"
		guidance = "# exact cloud guidance\n\nUse the locked tools.\n"
	)
	manifest := sharedtest.ContextTestExtensionManifest(name, version)
	archive := readPreparationArchive(t, map[string]string{
		"putnami.extension.json": manifest,
		"AI.md":                  guidance,
	})
	archiveSum := sha256.Sum256(archive)
	manifestSum := sha256.Sum256([]byte(manifest))

	hits := 0
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-Resolved-Version", version)
		_, _ = w.Write(archive)
	}))
	defer registry.Close()
	t.Setenv(extension.PutRegistryURLEnv, registry.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	credentialCalls := 0
	originalCredentialResolver := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) {
		credentialCalls++
		return "must-not-be-used", ""
	}
	t.Cleanup(func() { extension.ResolveRegistryToken = originalCredentialResolver })

	newWorkspace := func() (string, *wsproto.Config, map[string]string) {
		root := t.TempDir()
		cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "stable"}}}
		lf := lockfile.NewLockFile()
		lf.SetExtension(name, lockfile.LockEntry{
			Version: version, ManifestHash: hex.EncodeToString(manifestSum[:]),
			Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): hex.EncodeToString(archiveSum[:])},
		})
		if err := lockfile.WriteLockFile(root, lf); err != nil {
			t.Fatal(err)
		}
		owned := map[string]string{
			".agents/constraints.md":                     "# human constraints\nkeep\n",
			".putnami/agent-artifacts/workflows.json":    `{"owned":"receipt"}`,
			".putnami/cloud-existing-session-marker.txt": "keep-session",
		}
		for path, content := range owned {
			full := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root, cfg, owned
	}

	firstRoot, firstCfg, firstOwned := newWorkspace()
	lockBefore, err := os.ReadFile(filepath.Join(firstRoot, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	first := PrepareReadOnly(context.Background(), firstRoot, firstCfg)
	if len(first.Issues) != 0 || len(first.Extensions) != 1 {
		t.Fatalf("cold preparation = %+v, want one exact extension", first)
	}
	if got, err := os.ReadFile(filepath.Join(first.Extensions[0].Root, "AI.md")); err != nil || string(got) != guidance {
		t.Fatalf("cold guidance = %q, %v", got, err)
	}
	if hits != 1 {
		t.Fatalf("cold registry hits = %d, want 1", hits)
	}
	if credentialCalls != 0 {
		t.Fatalf("read preparation invoked mutating credential seam %d time(s)", credentialCalls)
	}

	// A second call and a separate worktree both reuse the verified CAS. The
	// registry must see no additional request.
	second := PrepareReadOnly(context.Background(), firstRoot, firstCfg)
	if len(second.Issues) != 0 || hits != 1 {
		t.Fatalf("warm preparation = %+v, registry hits %d", second, hits)
	}
	secondRoot, secondCfg, secondOwned := newWorkspace()
	newTree := PrepareReadOnly(context.Background(), secondRoot, secondCfg)
	if len(newTree.Issues) != 0 || len(newTree.Extensions) != 1 || hits != 1 {
		t.Fatalf("new-worktree preparation = %+v, registry hits %d", newTree, hits)
	}

	lockAfter, err := os.ReadFile(filepath.Join(firstRoot, lockfile.LockFilename))
	if err != nil || !bytes.Equal(lockBefore, lockAfter) {
		t.Fatalf("read preparation changed the lock: %v", err)
	}
	for root, files := range map[string]map[string]string{firstRoot: firstOwned, secondRoot: secondOwned} {
		for path, want := range files {
			got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil || string(got) != want {
				t.Errorf("%s changed: got %q, err %v", path, got, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, ".putnami", "install-state.json")); !os.IsNotExist(err) {
			t.Errorf("read preparation ran the full install lifecycle: stat err %v", err)
		}
	}
}

func TestPrepareReadOnlyMissingLockAndOfflineArtifactAreExplicit(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "offline-missing-artifacts-degrade-explicitly-without-latest")
	const name = "@putnami/cloud"
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "latest"}}}

	withoutLock := PrepareReadOnly(context.Background(), t.TempDir(), cfg)
	if len(withoutLock.Extensions) != 0 || len(withoutLock.Issues) != 1 || withoutLock.Issues[0].Name != name {
		t.Fatalf("missing-lock preparation = %+v", withoutLock)
	}

	root := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{Version: "1.2.3"})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extension.PutRegistryURLEnv, "http://127.0.0.1:1")
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	offline := PrepareReadOnly(context.Background(), root, cfg)
	if len(offline.Extensions) != 0 || len(offline.Issues) != 1 {
		t.Fatalf("offline preparation = %+v", offline)
	}
	if offline.Issues[0].Version != "1.2.3" {
		t.Fatalf("offline issue lost exact version: %+v", offline.Issues[0])
	}
}

func TestPrepareReadOnlyUnreadableLockMarksEveryRegistryExtensionUnavailable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, lockfile.LockFilename), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/cloud": "stable",
		"@putnami/go":    "stable",
		"/local/ext":     "workspace",
	}}}
	report := PrepareReadOnly(context.Background(), root, cfg)
	if len(report.Extensions) != 0 || len(report.Issues) != 2 {
		t.Fatalf("unreadable-lock preparation = %+v", report)
	}
	for i, want := range []string{"@putnami/cloud", "@putnami/go"} {
		if report.Issues[i].Name != want || !strings.Contains(report.Issues[i].Reason, lockfile.LockFilename) {
			t.Errorf("issue %d = %+v, want named %s lock failure", i, report.Issues[i], want)
		}
	}
}

func TestPrepareReadOnlyUnsafeInstallCannotPublishUnverifiedArtifact(t *testing.T) {
	const (
		name    = "@putnami/cloud"
		version = "1.2.3"
	)
	manifest := sharedtest.ContextTestExtensionManifest(name, version)
	archive := readPreparationArchive(t, map[string]string{
		"putnami.extension.json": manifest,
		"AI.md":                  "# unverified\n",
	})
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", version)
		_, _ = w.Write(archive)
	}))
	defer registry.Close()

	root := t.TempDir()
	storeRoot := t.TempDir()
	manifestSum := sha256.Sum256([]byte(manifest))
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{Version: version, ManifestHash: hex.EncodeToString(manifestSum[:])})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "stable"}}}
	t.Setenv(extension.PutRegistryURLEnv, registry.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv(extension.UnsafeInstallEnv, "1")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)

	report := PrepareReadOnly(context.Background(), root, cfg)
	if len(report.Extensions) != 0 || len(report.Issues) != 1 || !strings.Contains(report.Issues[0].Reason, "not verified") {
		t.Fatalf("unsafe preparation = %+v, want explicit unverified issue", report)
	}
	if _, err := os.Stat(layout.StableDir(root, layout.Extensions, name)); !os.IsNotExist(err) {
		t.Fatalf("unsafe preparation published stable link: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(storeRoot, "sha256")); err == nil && len(entries) != 0 {
		t.Fatalf("unsafe preparation published %d CAS shard(s)", len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func readPreparationArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"putnami.extension.json", "AI.md"} {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
