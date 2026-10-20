package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func fakeWorkspace(projects ...*workspace.Project) *workspace.Workspace {
	return &workspace.Workspace{Projects: projects}
}

// editableWorkspace holds api and worker, whose extension declares deps-edit,
// and web, whose extension does not.
func editableWorkspace() (*workspace.Workspace, map[string]string) {
	ws := fakeWorkspace(
		&workspace.Project{ID: "/api", Name: "api", Path: "api", Extensions: []string{"@acme/go"}},
		&workspace.Project{ID: "/web", Name: "web", Path: "web", Extensions: []string{"@acme/web"}},
		&workspace.Project{ID: "/worker", Name: "worker", Path: "worker", Extensions: []string{"@acme/go"}},
	)
	return ws, map[string]string{"/api": "@acme/go", "/worker": "@acme/go"}
}

func TestDepsEditTarget_SoleEditableProjectAutoSelected(t *testing.T) {
	t.Parallel()
	ws, editors := editableWorkspace()
	delete(editors, "/worker")
	got, err := depsEditTarget(ws, editors, "add", "")
	if err != nil || got.Name != "api" {
		t.Fatalf("depsEditTarget = %v, %v; want the sole project with an editor, api", got, err)
	}
}

func TestDepsEditTarget_AmbiguousRequiresSelector(t *testing.T) {
	t.Parallel()
	ws, editors := editableWorkspace()
	_, err := depsEditTarget(ws, editors, "add", "")
	if err == nil || !strings.Contains(err.Error(), "several projects") || !strings.Contains(err.Error(), "(api, worker)") {
		t.Fatalf("want an ambiguity error listing api and worker, got %v", err)
	}
}

func TestDepsEditTarget_SelectorPicksTheNamedProject(t *testing.T) {
	t.Parallel()
	ws, editors := editableWorkspace()
	for _, selector := range []string{"worker", "/worker"} {
		got, err := depsEditTarget(ws, editors, "remove", selector)
		if err != nil || got.Name != "worker" {
			t.Fatalf("depsEditTarget(%q) = %v, %v; want worker", selector, got, err)
		}
	}
}

func TestDepsEditTarget_RefusesAProjectWithoutAnEditor(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "deps-edit-runs-in-the-project-extension",
		"deps-refuses-a-project-whose-extensions-declare-no-deps-edit")
	ws, editors := editableWorkspace()
	_, err := depsEditTarget(ws, editors, "add", "web")
	if err == nil {
		t.Fatal("depsEditTarget selected a project whose extensions declare no deps-edit job")
	}
	for _, want := range []string{"deps add cannot edit web", "deps-edit", "@acme/web", "putnami deps install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestDepsEditTarget_NoEditableProjectErrors(t *testing.T) {
	t.Parallel()
	ws, _ := editableWorkspace()
	_, err := depsEditTarget(ws, nil, "add", "")
	if err == nil || !strings.Contains(err.Error(), "no project of the workspace") {
		t.Fatalf("want a no-editable-project error, got %v", err)
	}
}

func TestDepsEditTarget_UnknownOrSeveralSelectorsError(t *testing.T) {
	t.Parallel()
	ws, editors := editableWorkspace()
	if _, err := depsEditTarget(ws, editors, "add", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
	if _, err := depsEditTarget(ws, editors, "add", "api,worker"); err == nil || !strings.Contains(err.Error(), "a single project") {
		t.Fatalf("want a single-project error, got %v", err)
	}
}

func TestFindGoWork_WalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got := shared.FindGoWork(nested)
	want := filepath.Join(root, "go.work")
	if got != want {
		t.Errorf("FindGoWork(%q) = %q, want %q", nested, got, want)
	}
	if none := shared.FindGoWork(t.TempDir()); none != "" {
		t.Errorf("FindGoWork with no go.work = %q, want empty", none)
	}
}

func TestGoCommandEnv_DropsInheritedAndPointsAtGoWork(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off") // a leaked GOWORK=off must not survive
	env := shared.GoCommandEnv(root)

	var goworks []string
	for _, e := range env {
		if strings.HasPrefix(e, "GOWORK=") {
			goworks = append(goworks, e)
		}
	}
	want := "GOWORK=" + filepath.Join(root, "go.work")
	if len(goworks) != 1 || goworks[0] != want {
		t.Errorf("GOWORK entries = %v, want exactly [%q]", goworks, want)
	}
}

// FormatJobNames renders the "Available jobs:" line, so its order must not
// depend on Go's randomized map iteration: the same broken workspace has to
// produce the same message twice in a row.
func TestFormatJobNames(t *testing.T) {
	jobMap := map[string][]*extension.JobDefinition{
		"build": nil,
		"test":  nil,
		"lint":  nil,
	}

	result := FormatJobNames(jobMap)
	if result != "build, lint, test" {
		t.Errorf("FormatJobNames() = %q, want %q", result, "build, lint, test")
	}
	for i := 0; i < 20; i++ {
		if again := FormatJobNames(jobMap); again != result {
			t.Fatalf("FormatJobNames() is not deterministic: %q then %q", result, again)
		}
	}
}

func TestFormatJobNamesEmpty(t *testing.T) {
	result := FormatJobNames(map[string][]*extension.JobDefinition{})
	if result != "" {
		t.Errorf("FormatJobNames(empty) = %q, want empty", result)
	}
}
