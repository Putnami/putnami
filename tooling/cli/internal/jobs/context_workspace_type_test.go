package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// typedMembership returns a workspace with a Go library the provider probe
// classified (its putnami.json authors no type), an application whose
// putnami.json authors its type, and a project nothing classified.
func typedMembership(libraryType string) *workspace.Workspace {
	library := &workspace.Project{ID: "/go/lib", Path: "go/lib", Config: &wsproto.ProjectConfig{}}
	workspace.ResolveProjectIdentity(library, &wsproto.MergedProject{
		Path: "go/lib", SourceName: "example.com/lib", Type: libraryType,
	})
	app := &workspace.Project{ID: "/go/app", Path: "go/app",
		Config: &wsproto.ProjectConfig{Name: "example.com/app", Type: "application"}}
	workspace.ResolveProjectIdentity(app, nil)
	plain := &workspace.Project{ID: "/tools/plain", Name: "plain", Path: "tools/plain"}
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{library, app, plain})
	ws.Name = "ws"
	return ws
}

// A task that acts on the workspace's deployable workloads reads each member's
// resolved type from workspaceProjects. The authored config alone cannot answer
// it: a Go module's type comes from the provider probe, so its putnami.json
// authors none. Every entry carries a type, the workspace default included, so
// an entry without one comes from a producer that predates the member. The
// selection and the dependency closure carry none.
func TestBuildJobContext_WorkspaceProjectsCarryTheResolvedType(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-context-names-member-types", "each-workspace-member-names-its-resolved-type")
	ws := typedMembership("library")
	app := ws.ProjectByID("/go/app")
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          app,
		SelectedProjects: []*workspace.Project{app},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/cloud", Path: "/workspace/cloud"},
		JobDef:           &extension.JobDefinition{Name: "cloud-infra-plan", ExtensionName: "@putnami/cloud"},
	}, nil, nil, nil)

	types := map[string]string{}
	for _, ref := range ctx.WorkspaceProjects {
		types[ref.ID] = ref.Type
	}
	for id, want := range map[string]string{"/go/app": "application", "/go/lib": "library", "/tools/plain": "application"} {
		if got := types[id]; got != want {
			t.Errorf("workspaceProjects %s type = %q, want %q", id, got, want)
		}
	}

	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("a context carrying project types violates the job protocol: %v", diags)
	}
	for _, ref := range parsed.WorkspaceProjects {
		if ref.Type != types[ref.ID] {
			t.Errorf("parsed %s type = %q, want the produced %q", ref.ID, ref.Type, types[ref.ID])
		}
	}

	if len(ctx.Project.DependencyClosure) == 0 {
		t.Fatal("dependencyClosure is empty, want at least the project itself")
	}
	for _, ref := range ctx.Project.DependencyClosure {
		if ref.Type != "" {
			t.Errorf("dependencyClosure %s type = %q, want none", ref.ID, ref.Type)
		}
	}
	if len(ctx.SelectedProjects) == 0 {
		t.Fatal("selectedProjects is empty, want the selected application")
	}
	selected, err := json.Marshal(ctx.SelectedProjects)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(selected), `"type"`) {
		t.Errorf("selectedProjects = %s, want no type", selected)
	}
}

// A workspace with no projects still answers: the context names an empty
// workspaceProjects array, so a reader tells "no projects" from a CLI too old
// to write the member.
func TestBuildJobContext_WorkspaceProjectsNamesAnEmptyList(t *testing.T) {
	t.Parallel()
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, nil)
	ws.Name = "ws"
	own := &workspace.Project{ID: "/tools/own", Name: "own", Path: "tools/own"}
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          own,
		SelectedProjects: []*workspace.Project{own},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/cloud", Path: "/workspace/cloud"},
		JobDef:           &extension.JobDefinition{Name: "cloud-infra-plan", ExtensionName: "@putnami/cloud"},
	}, nil, nil, nil)

	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["workspaceProjects"]); got != "[]" {
		t.Errorf("workspaceProjects = %q, want []", got)
	}
	if _, diags := protocoljob.ParseAndValidate(data); diag.HasErrors(diags) {
		t.Errorf("a context with an empty membership violates the job protocol: %v", diags)
	}
}
