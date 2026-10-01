package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// writeGoMod creates <root>/<rel>/go.mod so the project reads as a Go module.
func writeGoMod(t *testing.T, root, rel string) {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeWorkspace(projects ...*workspace.Project) *workspace.Workspace {
	return &workspace.Workspace{Projects: projects}
}

func TestResolveGoDepsProject_SoleGoModuleAutoSelected(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, "api")
	ws := fakeWorkspace(
		&workspace.Project{Name: "api", Path: "api"},
		&workspace.Project{Name: "web", Path: "web"}, // no go.mod → not a Go module
	)
	got, err := resolveGoDepsProject(root, ws, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "api" {
		t.Errorf("auto-selected %q, want the sole Go module \"api\"", got.Name)
	}
}

func TestResolveGoDepsProject_AmbiguousRequiresSelector(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, "api")
	writeGoMod(t, root, "worker")
	ws := fakeWorkspace(
		&workspace.Project{Name: "api", Path: "api"},
		&workspace.Project{Name: "worker", Path: "worker"},
	)
	_, err := resolveGoDepsProject(root, ws, "")
	if err == nil || !strings.Contains(err.Error(), "multiple Go modules") {
		t.Fatalf("want an ambiguity error listing modules, got %v", err)
	}
}

func TestResolveGoDepsProject_SelectorPicksNamedModule(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, "api")
	writeGoMod(t, root, "worker")
	ws := fakeWorkspace(
		&workspace.Project{Name: "api", Path: "api"},
		&workspace.Project{Name: "worker", Path: "worker"},
	)
	got, err := resolveGoDepsProject(root, ws, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "worker" {
		t.Errorf("selector picked %q, want \"worker\"", got.Name)
	}
}

func TestResolveGoDepsProject_NonGoSelectorErrors(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, "api")
	ws := fakeWorkspace(
		&workspace.Project{Name: "api", Path: "api"},
		&workspace.Project{Name: "web", Path: "web"}, // TS project, no go.mod
	)
	_, err := resolveGoDepsProject(root, ws, "web")
	if err == nil || !strings.Contains(err.Error(), "not a Go module") {
		t.Fatalf("want a not-a-Go-module error, got %v", err)
	}
}

func TestResolveGoDepsProject_NoGoModuleErrors(t *testing.T) {
	root := t.TempDir()
	ws := fakeWorkspace(&workspace.Project{Name: "web", Path: "web"})
	_, err := resolveGoDepsProject(root, ws, "")
	if err == nil || !strings.Contains(err.Error(), "no Go module") {
		t.Fatalf("want a no-Go-module error, got %v", err)
	}
}

func TestResolveGoDepsProject_UnknownSelectorErrors(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, "api")
	ws := fakeWorkspace(&workspace.Project{Name: "api", Path: "api"})
	if _, err := resolveGoDepsProject(root, ws, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
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
