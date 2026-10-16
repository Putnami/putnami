package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
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
// authors none.
func TestBuildJobContext_WorkspaceProjectsCarryTheResolvedType(t *testing.T) {
	t.Parallel()
	ws := typedMembership("library")
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:   ws.ProjectByID("/go/app"),
		Extension: &extension.ExtensionDescription{Name: "@putnami/cloud", Path: "/workspace/cloud"},
		JobDef:    &extension.JobDefinition{Name: "cloud-infra-plan", ExtensionName: "@putnami/cloud"},
	}, nil, nil, nil)

	types := map[string]string{}
	for _, ref := range ctx.WorkspaceProjects {
		types[ref.ID] = ref.Type
	}
	for id, want := range map[string]string{"/go/app": "application", "/go/lib": "library", "/tools/plain": ""} {
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
	plain, err := json.Marshal(ctx.WorkspaceProjects[2])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), `"type"`) {
		t.Errorf("unclassified member = %s, want type omitted", plain)
	}
}

// The membership a task keyed on the Git candidate cut hashes is the one its
// context carries, so a member that changes only its type moves the key.
func TestWorkspaceMembershipDigestCoversTheResolvedType(t *testing.T) {
	t.Parallel()
	digest := func(libraryType string) string {
		t.Helper()
		value, err := workspaceMembershipDigest(typedMembership(libraryType))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest("library") == digest("application") {
		t.Error("the membership digest stayed put when a member's type changed")
	}
	if digest("library") != digest("library") {
		t.Error("the membership digest moved over one membership")
	}
}
