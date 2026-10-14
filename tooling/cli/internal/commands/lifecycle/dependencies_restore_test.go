package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const dependenciesFeature = "cli/extension-dependencies"

// dependencyGraphRegistry serves one build per extension and counts the
// downloads of each.
type dependencyGraphRegistry struct {
	mu        sync.Mutex
	downloads map[string]int
}

func (r *dependencyGraphRegistry) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloads[name]
}

// dependencyGraphWorkspace is a workspace that declares @acme/app, whose
// manifest names @acme/dev and @acme/tool. The lock pins @acme/app and
// @acme/dev, not @acme/tool. The registry serves all three.
func dependencyGraphWorkspace(t *testing.T) (string, *wsproto.Config, *dependencyGraphRegistry) {
	t.Helper()
	manifests := map[string]string{
		"@acme/app":  `{"name":"@acme/app","version":"1.0.0","cliContract":8,"extensionDependencies":{"@acme/dev":"^1.0.0","@acme/tool":""},"commands":{}}`,
		"@acme/dev":  `{"name":"@acme/dev","version":"1.0.0","commands":{}}`,
		"@acme/tool": `{"name":"@acme/tool","version":"1.0.0","commands":{}}`,
	}
	archives := map[string][]byte{}
	for name, manifest := range manifests {
		archives[name] = buildManifestArchive(t, extension.ManifestFilename, manifest)
	}
	registry := &dependencyGraphRegistry{downloads: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.NotFound(w, req)
			return
		}
		name := fmt.Sprintf("@%s/%s", parts[len(parts)-3], parts[len(parts)-2])
		archive, ok := archives[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		registry.mu.Lock()
		registry.downloads[name]++
		registry.mu.Unlock()
		w.Header().Set("X-Resolved-Version", "1.0.0")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	resolveToken := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryToken = resolveToken })

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, wsproto.WorkspaceConfigFilename), []byte(`{"extensions":{"@acme/app":""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	for _, name := range []string{"@acme/app", "@acme/dev"} {
		lf.SetExtension(name, lockfile.LockEntry{
			Version: "1.0.0", ManifestHash: sha256Hex([]byte(manifests[name])),
			Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): sha256Hex(archives[name])},
		})
	}
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@acme/app": ""}}}
	return ws, cfg, registry
}

// checkDependencyRestore checks a restore of dependencyGraphWorkspace: the
// pinned dependency is installed, the unpinned one is never requested, and
// the lock is unchanged.
func checkDependencyRestore(t *testing.T, ws string, registry *dependencyGraphRegistry, lockBefore []byte) {
	t.Helper()
	if got := registry.count("@acme/dev"); got != 1 {
		t.Errorf("@acme/dev downloads = %d, want its locked build restored once", got)
	}
	if _, err := os.Stat(filepath.Join(layout.StableDir(ws, layout.Extensions, "@acme/dev"), extension.ManifestFilename)); err != nil {
		t.Errorf("the locked dependency is not linked: %v", err)
	}
	if got := registry.count("@acme/tool"); got != 0 {
		t.Errorf("@acme/tool downloads = %d, want the unpinned dependency never resolved", got)
	}
	if _, err := os.Lstat(layout.StableDir(ws, layout.Extensions, "@acme/tool")); !os.IsNotExist(err) {
		t.Errorf("the unpinned dependency was installed: %v", err)
	}
	if lockAfter, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename)); err != nil || !bytes.Equal(lockAfter, lockBefore) {
		t.Errorf("the restore changed the lock: %v", err)
	}
}

// The restore of a new worktree and the read preparation restore each
// dependency the lock pins, below the extension the workspace declares. A
// dependency the lock does not pin is never resolved: each reports it with
// its dependent.
func TestRestoreAndReadPreparationFollowTheGraph(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "restore-follows-the-lock", "restore-and-read-preparation-follow-the-graph")
	spectest.Proves(t, dependenciesFeature, "restore-follows-the-lock", "an-unpinned-dependency-is-never-resolved-implicitly")
	const unpinned = "@acme/app requires @acme/tool: putnami.lock.json does not pin it; run `putnami install`"

	t.Run("read preparation", func(t *testing.T) {
		ws, cfg, registry := dependencyGraphWorkspace(t)
		lockBefore, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
		if err != nil {
			t.Fatal(err)
		}
		report := PrepareReadOnly(context.Background(), ws, cfg)
		if len(report.Extensions) != 1 || report.Extensions[0].Name != "@acme/app" {
			t.Errorf("prepared extensions = %+v, want the declared one", report.Extensions)
		}
		if len(report.Issues) != 1 || report.Issues[0].Name != "@acme/tool" || report.Issues[0].Reason != unpinned {
			t.Errorf("issues = %+v, want @acme/tool named with its dependent", report.Issues)
		}
		checkDependencyRestore(t, ws, registry, lockBefore)
	})

	t.Run("restore", func(t *testing.T) {
		ws, cfg, registry := dependencyGraphWorkspace(t)
		t.Setenv(artifactsEnsuredEnv, "")
		agentWorkflows := ensureAgentWorkflows
		ensureAgentWorkflows = func(context.Context, string, *wsproto.Config) error { return nil }
		t.Cleanup(func() { ensureAgentWorkflows = agentWorkflows })
		lockBefore, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
		if err != nil {
			t.Fatal(err)
		}
		err = EnsureArtifacts(context.Background(), ws, cfg)
		if err == nil || err.Error() != unpinned {
			t.Errorf("restore = %v, want %q", err, unpinned)
		}
		checkDependencyRestore(t, ws, registry, lockBefore)
	})
}
