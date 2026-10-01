package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/mapgen"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// mapFixtureWorkspace builds a one-project workspace on disk and returns it plus
// the project, so the finalizer can be exercised end to end without a scheduler.
func mapFixtureWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	root := t.TempDir()
	abs := filepath.Join(root, "svc", "api")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(abs, "putnami.json"), []byte(`{"name":"example/api"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}
	p := &workspace.Project{ID: "/svc/api", Name: "example/api", Path: "svc/api", Type: "library"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "example"}, []*workspace.Project{p})
	return ws, p
}

// TestContextMapFinalizer_RunsInEveryWorkspace pins the removal of the
// adoption gate: the map is gitignored CLI state, so writing it is harmless
// anywhere and no workspace has to opt in first.
func TestContextMapFinalizer_RunsInEveryWorkspace(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("PUTNAMI_CONTEXT_MAP", "")
	ws, p := mapFixtureWorkspace(t)

	finalize := contextMapFinalizer(&Request{Commands: []string{"build"}}, ws, []*workspace.Project{p})
	if finalize == nil {
		t.Fatal("a build in a workspace that never adopted a map carried no finalizer")
	}
	results := map[string]*jobs.JobResult{"/svc/api:build": {Status: "success"}}
	_ = captureStderr(t, func() { finalize(results) })

	if len(results) != 1 {
		t.Fatalf("a successful run added a result: %v", results)
	}
	for _, rel := range []string{mapgen.JSONPath, mapgen.MarkdownPath} {
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("build did not write %s: %v", rel, err)
		}
	}
}

// TestContextMapFinalizer_AttachesOnlyToBuild pins the attachment point: build
// sessions get the map, other verbs do not, CI resolves to off (nothing is
// committed, and a runner has no map consumer), and the explicit off disables it
// everywhere.
func TestContextMapFinalizer_AttachesOnlyToBuild(t *testing.T) {
	ws, p := mapFixtureWorkspace(t)
	selected := []*workspace.Project{p}

	t.Run("non-build session", func(t *testing.T) {
		t.Setenv("CI", "")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "")
		req := &Request{Commands: []string{"lint", "test"}}
		if fin := contextMapFinalizer(req, ws, selected); fin != nil {
			t.Error("a session without build must not carry the map finalizer")
		}
	})
	t.Run("multi-command session including build", func(t *testing.T) {
		t.Setenv("CI", "")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "")
		req := &Request{Commands: []string{"lint", "test", "build"}}
		if fin := contextMapFinalizer(req, ws, selected); fin == nil {
			t.Error("lint,test,build must carry the map finalizer")
		}
	})
	t.Run("ci is off", func(t *testing.T) {
		t.Setenv("CI", "1")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "")
		req := &Request{Commands: []string{"build"}}
		if fin := contextMapFinalizer(req, ws, selected); fin != nil {
			t.Error("a CI build must not carry the map finalizer")
		}
	})
	t.Run("explicit write beats ci", func(t *testing.T) {
		t.Setenv("CI", "1")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "write")
		req := &Request{Commands: []string{"build"}}
		if fin := contextMapFinalizer(req, ws, selected); fin == nil {
			t.Error("an explicit write must run under CI")
		}
	})
	t.Run("explicitly off", func(t *testing.T) {
		t.Setenv("CI", "")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "off")
		req := &Request{Commands: []string{"build"}}
		if fin := contextMapFinalizer(req, ws, selected); fin != nil {
			t.Error("PUTNAMI_CONTEXT_MAP=off must disable the attachment")
		}
	})
	t.Run("no workspace", func(t *testing.T) {
		t.Setenv("CI", "")
		t.Setenv("PUTNAMI_CONTEXT_MAP", "")
		req := &Request{Commands: []string{"build"}}
		if fin := contextMapFinalizer(req, nil, nil); fin != nil {
			t.Error("a run with no loaded workspace must not carry the map finalizer")
		}
	})
}

// TestContextMapFinalizer_RefreshesAfterASuccessfulBuild is the local behavior:
// `putnami build` leaves the ephemeral map correct as a side effect, adds no
// result to the session, and a repeat build over an unchanged tree writes
// nothing at all.
func TestContextMapFinalizer_RefreshesAfterASuccessfulBuild(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("PUTNAMI_CONTEXT_MAP", "")
	ws, p := mapFixtureWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.Root, "svc", "api", "README.md"),
		[]byte("# API\n\nServes tasks.\n"), 0o644); err != nil {
		t.Fatalf("edit the project: %v", err)
	}

	run := func() string {
		t.Helper()
		finalize := contextMapFinalizer(&Request{Commands: []string{"build"}}, ws, []*workspace.Project{p})
		if finalize == nil {
			t.Fatal("build must carry the map finalizer")
		}
		results := map[string]*jobs.JobResult{"/svc/api:build": {Status: "success"}}
		stderr := captureStderr(t, func() { finalize(results) })
		if len(results) != 1 {
			t.Fatalf("a successful run added a result: %v", results)
		}
		return stderr
	}

	if stderr := run(); !strings.Contains(stderr, "refreshed the workspace map") {
		t.Errorf("the first build did not report the refresh: %q", stderr)
	}
	mapPath := filepath.Join(ws.Root, filepath.FromSlash(mapgen.JSONPath))
	before, err := os.Stat(mapPath)
	if err != nil {
		t.Fatalf("stat map: %v", err)
	}
	if data, readErr := os.ReadFile(mapPath); readErr != nil || !strings.Contains(string(data), "Serves tasks.") {
		t.Errorf("build did not refresh the map: %v", readErr)
	}

	// Unchanged tree: one digest sweep, zero writes, and nothing said about it.
	if stderr := run(); stderr != "" {
		t.Errorf("a no-op refresh printed %q, want silence", stderr)
	}
	after, err := os.Stat(mapPath)
	if err != nil {
		t.Fatalf("stat map: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a repeat build rewrote an unchanged map")
	}
}

// TestContextMapFinalizer_SkipsAFailedSession pins the "must not turn a build
// failure into a map problem" rule from both directions: a failed task and a
// canceled one both leave the map alone.
func TestContextMapFinalizer_SkipsAFailedSession(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("PUTNAMI_CONTEXT_MAP", "")

	for _, status := range []string{"failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			ws, p := mapFixtureWorkspace(t)

			finalize := contextMapFinalizer(&Request{Commands: []string{"build"}}, ws, []*workspace.Project{p})
			results := map[string]*jobs.JobResult{
				"/svc/api:build": {Status: status},
				"/svc/api:lint":  {Status: "success"},
			}
			_ = captureStderr(t, func() { finalize(results) })

			if len(results) != 2 {
				t.Errorf("the finalizer contributed a result to an unsuccessful session: %v", results)
			}
			if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(mapgen.JSONPath))); !os.IsNotExist(err) {
				t.Errorf("an unsuccessful session wrote the map (stat err = %v)", err)
			}
		})
	}
}

// TestContextMapFinalizer_AnUnwritableStateDirectoryWarnsInsteadOfFailing pins
// the other half of the "no red builds" rule: the map is a convenience the MCP
// tool rebuilds in memory, so a state directory the CLI cannot write costs a
// stderr warning, never the session.
func TestContextMapFinalizer_AnUnwritableStateDirectoryWarnsInsteadOfFailing(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("PUTNAMI_CONTEXT_MAP", "")
	ws, p := mapFixtureWorkspace(t)
	// A regular file where the state directory belongs makes every write under
	// it fail, portably.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(ws.Root, filepath.FromSlash(mapgen.FragmentDir))), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, filepath.FromSlash(mapgen.FragmentDir)),
		[]byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	finalize := contextMapFinalizer(&Request{Commands: []string{"build"}}, ws, []*workspace.Project{p})
	results := map[string]*jobs.JobResult{"/svc/api:build": {Status: "success"}}
	stderr := captureStderr(t, func() { finalize(results) })

	if len(results) != 1 {
		t.Fatalf("an unwritable map state directory failed the session: %v", results)
	}
	if !strings.Contains(stderr, "could not refresh the workspace map") {
		t.Errorf("the failure was swallowed silently: %q", stderr)
	}
}
