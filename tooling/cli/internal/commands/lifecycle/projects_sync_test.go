package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func writeProjectFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPartitionByScope(t *testing.T) {
	scopes := []string{"a", "a/b"} // longest match must win
	paths := []string{"a/b/proj1", "a/proj2", "root/proj3"}

	scopeMap, root := partitionByScope(scopes, paths)

	if got := scopeMap["a/b"]; len(got) != 1 || got[0] != "proj1" {
		t.Errorf("scope a/b = %v, want [proj1]", got)
	}
	if got := scopeMap["a"]; len(got) != 1 || got[0] != "proj2" {
		t.Errorf("scope a = %v, want [proj2]", got)
	}
	if len(root) != 1 || root[0] != "root/proj3" {
		t.Errorf("root = %v, want [root/proj3]", root)
	}
}

func TestUpdateScopeProjects_AddRemove(t *testing.T) {
	ws := t.TempDir()
	scopeCfg := filepath.Join(ws, "scope1", "putnami.json")
	writeProjectFile(t, scopeCfg, `{"includes":["a","b"]}`)

	if err := updateScopeProjects(ws, "scope1", []string{"c"}, []string{"a"}); err != nil {
		t.Fatalf("updateScopeProjects: %v", err)
	}

	got := readText(t, scopeCfg)
	if !strings.Contains(got, `"b"`) || !strings.Contains(got, `"c"`) {
		t.Errorf("expected includes to contain b and c:\n%s", got)
	}
	if strings.Contains(got, `"a"`) {
		t.Errorf("expected 'a' to be removed:\n%s", got)
	}
}

func TestProjectsSync_AddsDiscoveredProjects(t *testing.T) {
	ws := t.TempDir()
	wsCfg := filepath.Join(ws, "putnami.workspace.json")
	writeProjectFile(t, wsCfg, `{"includes":[]}`)
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)
	writeProjectFile(t, filepath.Join(ws, "svc-b", "putnami.json"), `{"name":"svc-b"}`)

	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	got := readText(t, wsCfg)
	if !strings.Contains(got, "svc-a") || !strings.Contains(got, "svc-b") {
		t.Errorf("workspace config not updated with discovered projects:\n%s", got)
	}
}

func TestProjectsSync_DryRunWritesNothing(t *testing.T) {
	ws := t.TempDir()
	wsCfg := filepath.Join(ws, "putnami.workspace.json")
	writeProjectFile(t, wsCfg, `{"includes":[]}`)
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)

	before := readText(t, wsCfg)
	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, true, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync dry-run: %v", err)
	}

	if after := readText(t, wsCfg); after != before {
		t.Errorf("dry-run must not modify the workspace config:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestProjectsSync_AddsNewProjectToScopeConfig(t *testing.T) {
	ws := t.TempDir()
	// Root workspace includes an autonomous scope.
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":["myscope"]}`)
	// Scope config already declares one project.
	scopeCfg := filepath.Join(ws, "myscope", "putnami.json")
	writeProjectFile(t, scopeCfg, `{"includes":["existing"]}`)
	writeProjectFile(t, filepath.Join(ws, "myscope", "existing", "putnami.json"), `{"name":"existing"}`)
	// A new project on disk under the scope, not yet declared.
	writeProjectFile(t, filepath.Join(ws, "myscope", "newproj", "putnami.json"), `{"name":"newproj"}`)

	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	got := readText(t, scopeCfg)
	if !strings.Contains(got, "newproj") {
		t.Errorf("new project should be added to the scope config:\n%s", got)
	}
	if !strings.Contains(got, "existing") {
		t.Errorf("existing scope entry should be preserved:\n%s", got)
	}
}

func TestProjectsSync_PreservesTransparentGroupFolderPaths(t *testing.T) {
	ws := t.TempDir()
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":["identity"]}`)
	scopeCfg := filepath.Join(ws, "identity", "putnami.json")
	writeProjectFile(t, scopeCfg, `{"includes":["(libs)/identity-client"]}`)
	writeProjectFile(t, filepath.Join(ws, "identity", "(libs)", "identity-client", "putnami.json"), `{"name":"identity-client"}`)
	writeProjectFile(t, filepath.Join(ws, "identity", "(workloads)", "auth-server", "putnami.json"), `{"name":"auth-server"}`)

	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if got := readText(t, scopeCfg); !strings.Contains(got, `"(workloads)/auth-server"`) {
		t.Errorf("scope config must keep the physical grouped path:\n%s", got)
	}
}

// replaceClosureWorkspace builds a workspace already in membership sync whose
// go.work member `moda` is a LOCAL EXTENSION missing the replace for its
// workspace dependency `modb`.
//
// The extension part is what makes the fixture meaningful since slice C4b:
// core's closure repair is scoped to the extension modules it must prepare a
// runtime from, because that preparation is what a broken closure blocks. Every
// other module's closure belongs to the Go extension's own `workspace-sync`
// task.
func replaceClosureWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":["moda","modb"]}`)
	writeProjectFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse (\n\t./moda\n\t./modb\n)\n")
	writeProjectFile(t, filepath.Join(ws, "moda", "go.mod"), "module example.com/a\n\ngo 1.26\n\nrequire example.com/b v0.0.0\n")
	writeProjectFile(t, filepath.Join(ws, "moda", "putnami.json"), `{"name":"@fixture/moda"}`)
	writeProjectFile(t, filepath.Join(ws, "moda", "putnami.extension.json"), `{
  "name": "@fixture/moda",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": { "markers": ["go.mod"], "inputs": ["go.mod"], "syncTask": "workspace-sync-exec" },
  "commands": {
    "workspace-sync": {
      "description": "Synchronize native workspace state.",
      "visibility": "internal",
      "run": [{ "id": "workspace-sync", "task": "workspace-sync-exec" }]
    }
  },
  "tasks": { "workspace-sync-exec": { "kind": "command", "command": "echo", "cache": false } }
}`)
	writeProjectFile(t, filepath.Join(ws, "modb", "go.mod"), "module example.com/b\n\ngo 1.26\n")
	writeProjectFile(t, filepath.Join(ws, "modb", "putnami.json"), `{"name":"@fixture/modb"}`)
	workspace.InvalidateLoadCache(ws)
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })
	return ws
}

// syncJobEnv accepts every workspace job the sync hands to an extension. The
// fixture's sync task has no real runtime, and what these tests assert is what
// CORE did, not what an extension would have.
func syncJobEnv() LifecycleEnv {
	return LifecycleEnv{Out: os.Stdout, RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
}

func TestProjectsSync_RepairsTheBootstrapClosureOfALocalExtension(t *testing.T) {
	ws := replaceClosureWorkspace(t)

	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, syncJobEnv()); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	got := readText(t, filepath.Join(ws, "moda", "go.mod"))
	if !strings.Contains(got, "replace example.com/b => ../modb") {
		t.Errorf("the extension module should have gained the workspace replace:\n%s", got)
	}
}

func TestProjectsSync_DryRunLeavesGoModUntouched(t *testing.T) {
	ws := replaceClosureWorkspace(t)

	before := readText(t, filepath.Join(ws, "moda", "go.mod"))
	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, true, syncJobEnv()); err != nil {
		t.Fatalf("ProjectsSync dry-run: %v", err)
	}

	if after := readText(t, filepath.Join(ws, "moda", "go.mod")); after != before {
		t.Errorf("dry-run must not modify go.mod:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestProjectsSync_InSyncIsNoOp(t *testing.T) {
	ws := t.TempDir()
	wsCfg := filepath.Join(ws, "putnami.workspace.json")
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)
	writeProjectFile(t, wsCfg, `{"includes":["svc-a"]}`)

	cfg := wsproto.Load(ws)
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if got := readText(t, wsCfg); !strings.Contains(got, "svc-a") {
		t.Errorf("already-declared project should remain in config:\n%s", got)
	}
}

// `projects sync` is the full scan, so it is where the digest-authoritative
// workspace snapshot is (re)written. The snapshot must describe the
// tree the sync just reconciled.
func TestProjectsSync_WritesWorkspaceSnapshot(t *testing.T) {
	ws := t.TempDir()
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":[]}`)
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)

	cfg := wsproto.Load(ws)
	workspace.InvalidateLoadCache(ws)
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	snapshot, err := workspace.LoadSnapshot(ws)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snapshot == nil {
		t.Fatal("projects sync did not write .putnami/workspace-index.json")
	}
	if len(snapshot.Projects) == 0 || snapshot.IdentityDigest == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if verdict := snapshot.Validate(ws); !verdict.Valid {
		t.Errorf("the snapshot a sync just wrote is invalid: %+v", verdict)
	}
}

// Under --dry-run nothing is persisted, including the snapshot: a speculative
// run must not become the workspace's recorded identity.
func TestProjectsSync_DryRunWritesNoSnapshot(t *testing.T) {
	ws := t.TempDir()
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":[]}`)
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)

	cfg := wsproto.Load(ws)
	workspace.InvalidateLoadCache(ws)
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, true, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync dry-run: %v", err)
	}

	if _, err := os.Stat(workspace.SnapshotPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote the workspace snapshot (stat err = %v)", err)
	}
}

// A sync that changes nothing must not rewrite the snapshot: the persisted
// identity plus a content re-hash of the recorded metadata inputs already says
// it holds. Proven by pinning the file's modification time and observing that
// the second sync leaves it alone.
func TestProjectsSync_UnchangedTreeDoesNotRewriteSnapshot(t *testing.T) {
	ws := t.TempDir()
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":[]}`)
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a"}`)

	cfg := wsproto.Load(ws)
	workspace.InvalidateLoadCache(ws)
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })
	if err := ProjectsSync(context.Background(), ws, cfg, []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	snapshotFile := workspace.SnapshotPath(ws)
	pinned := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(snapshotFile, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	if err := ProjectsSync(context.Background(), ws, wsproto.Load(ws), []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	info, err := os.Stat(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("an unchanged tree rewrote the snapshot (mtime %v, want %v)", info.ModTime(), pinned)
	}

	// A metadata change must bring the write back.
	writeProjectFile(t, filepath.Join(ws, "svc-a", "putnami.json"), `{"name":"svc-a","tags":["go","extra"]}`)
	if err := ProjectsSync(context.Background(), ws, wsproto.Load(ws), []string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	info, err = os.Stat(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(pinned) {
		t.Error("a metadata change did not refresh the snapshot")
	}
}

// THE BOOTSTRAP EXCEPTION, NARROWED.
//
// The full workspace-replace closure moved to the Go extension's own
// `workspace-sync` task in C4a. What core kept is the smallest part of it that
// cannot be handed over: the modules whose runtime must be PREPARED before any
// extension can run at all, because preparing a local extension compiles it in
// module mode and a missing replace makes that compile resolve a placeholder
// version through the proxy.
//
// These two tests pin both halves of that narrowing — the repair still happens,
// and it happens ONLY for the bootstrap modules.
func TestProjectsSync_ConvergesTheBootstrapClosure(t *testing.T) {
	ws := replaceClosureWorkspace(t)

	if err := ProjectsSync(context.Background(), ws, wsproto.Load(ws),
		[]string{"--skip-install"}, false, syncJobEnv()); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}
	before := readText(t, filepath.Join(ws, "moda", "go.mod"))
	if !strings.Contains(before, "replace example.com/b => ../modb") {
		t.Fatalf("core did not write the missing workspace replace:\n%s", before)
	}

	// Converged: a second run finds nothing to do and rewrites nothing.
	if err := ProjectsSync(context.Background(), ws, wsproto.Load(ws),
		[]string{"--skip-install"}, false, syncJobEnv()); err != nil {
		t.Fatalf("second ProjectsSync: %v", err)
	}
	if after := readText(t, filepath.Join(ws, "moda", "go.mod")); after != before {
		t.Errorf("a converged closure was rewritten:\n%s", after)
	}
}

// A go.work member that is NOT an extension core must prepare is left to the Go
// extension's sync task. Two maintainers of one append-only codemod is how a
// go.mod starts gaining directives nobody asked for.
func TestProjectsSync_LeavesNonBootstrapModulesToTheExtension(t *testing.T) {
	ws := replaceClosureWorkspace(t)
	// modc is an ordinary Go project with the same missing replace.
	writeProjectFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"includes":["moda","modb","modc"]}`)
	writeProjectFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse (\n\t./moda\n\t./modb\n\t./modc\n)\n")
	writeProjectFile(t, filepath.Join(ws, "modc", "go.mod"), "module example.com/c\n\ngo 1.26\n\nrequire example.com/b v0.0.0\n")
	writeProjectFile(t, filepath.Join(ws, "modc", "putnami.json"), `{"name":"@fixture/modc"}`)
	workspace.InvalidateLoadCache(ws)

	before := readText(t, filepath.Join(ws, "modc", "go.mod"))
	if err := ProjectsSync(context.Background(), ws, wsproto.Load(ws),
		[]string{"--skip-install"}, false, syncJobEnv()); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}
	if after := readText(t, filepath.Join(ws, "modc", "go.mod")); after != before {
		t.Errorf("core repaired a module it does not have to prepare:\n%s", after)
	}
}
