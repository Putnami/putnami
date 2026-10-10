package cloudcli

import (
	"path/filepath"
	"strings"
	"testing"
)

// tempWorkspace returns a temp dir with symlinks resolved, matching what
// findAppDir does internally (macOS exposes t.TempDir() under /var, a symlink
// to /private/var) so path equality assertions hold.
func tempWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	return root
}

// writeNamedProject writes a minimal putnami.json (name only) at
// <workspaceRoot>/<dir>, the layout findAppDir walks.
func writeNamedProject(t *testing.T, workspaceRoot, dir, name string) {
	t.Helper()
	appDir := filepath.Join(workspaceRoot, dir)
	mustMkdir(t, appDir)
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{"name": name})
}

func TestProjectNameTail(t *testing.T) {
	cases := map[string]string{
		"apps/api":        "api",
		"apps/put-server": "put-server",
		"events":          "events",
		"a/b/c":           "c",
	}
	for in, want := range cases {
		if got := projectNameTail(in); got != want {
			t.Errorf("projectNameTail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindAppDir_ExactNameWins(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")
	writeNamedProject(t, root, "apps/put-server", "apps/put-server")

	dir, err := findAppDir(root, "apps/api")
	if err != nil {
		t.Fatalf("findAppDir exact: %v", err)
	}
	if want := filepath.Join(root, "apps", "api"); dir != want {
		t.Fatalf("findAppDir exact = %q, want %q", dir, want)
	}
}

func TestFindAppDir_UnqualifiedTailResolves(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")
	writeNamedProject(t, root, "apps/put-server", "apps/put-server")

	// A bare tail such as `api` resolves the namespaced project.
	dir, err := findAppDir(root, "api")
	if err != nil {
		t.Fatalf("findAppDir tail: %v", err)
	}
	if want := filepath.Join(root, "apps", "api"); dir != want {
		t.Fatalf("findAppDir tail = %q, want %q", dir, want)
	}

	// And `put-server` resolves `apps/put-server`, confirming the fallback is general.
	dir, err = findAppDir(root, "put-server")
	if err != nil {
		t.Fatalf("findAppDir tail put: %v", err)
	}
	if want := filepath.Join(root, "apps", "put-server"); dir != want {
		t.Fatalf("findAppDir tail put = %q, want %q", dir, want)
	}
}

func TestFindAppDir_AmbiguousTailErrors(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/auth-server", "apps/auth-server")
	writeNamedProject(t, root, "events/auth-server", "events/auth-server")

	_, err := findAppDir(root, "auth-server")
	if err == nil {
		t.Fatal("findAppDir ambiguous tail: expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "apps/auth-server") || !strings.Contains(msg, "events/auth-server") {
		t.Fatalf("ambiguous error missing detail: %q", msg)
	}
}

func TestFindAppDir_ExactWinsOverTail(t *testing.T) {
	root := tempWorkspace(t)
	// A project literally named "server" must win over a tail match of
	// "other/server" — an exact name is never ambiguous.
	writeNamedProject(t, root, "server", "server")
	writeNamedProject(t, root, "other/server", "other/server")

	dir, err := findAppDir(root, "server")
	if err != nil {
		t.Fatalf("findAppDir exact-over-tail: %v", err)
	}
	if want := filepath.Join(root, "server"); dir != want {
		t.Fatalf("findAppDir exact-over-tail = %q, want %q", dir, want)
	}
}

func TestFindAppDir_NotFound(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")

	_, err := findAppDir(root, "nope")
	if err == nil {
		t.Fatal("findAppDir not-found: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no workspace project named") {
		t.Fatalf("not-found error unexpected: %q", err.Error())
	}
}

func TestCanonicalProjectName_TailReturnsFullName(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")

	name, err := canonicalProjectName(root, "api")
	if err != nil {
		t.Fatalf("canonicalProjectName: %v", err)
	}
	if name != "apps/api" {
		t.Fatalf("canonicalProjectName = %q, want apps/api", name)
	}
}

func TestResolveApp_CanonicalizesTailParam(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")

	got, err := resolveApp(map[string]any{"app": "api"}, root)
	if err != nil {
		t.Fatalf("resolveApp: %v", err)
	}
	if got != "apps/api" {
		t.Fatalf("resolveApp(api) = %q, want canonical apps/api", got)
	}
}

func TestResolveApp_UnknownParamReturnedVerbatim(t *testing.T) {
	root := tempWorkspace(t)
	writeNamedProject(t, root, "apps/api", "apps/api")

	// A token that matches no local project is returned as-is so the precise
	// not-found error surfaces at the point of use, not here.
	got, err := resolveApp(map[string]any{"app": "remote-only-app"}, root)
	if err != nil {
		t.Fatalf("resolveApp verbatim: %v", err)
	}
	if got != "remote-only-app" {
		t.Fatalf("resolveApp(remote-only-app) = %q, want remote-only-app", got)
	}
}
