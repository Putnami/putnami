package scheduler

import (
	"encoding/json"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// moduleGraphMembership returns a Go service that imports core, keeps an
// `// indirect` require of client that only its module graph needs, and
// declares shared in an entry no provider attributed.
func moduleGraphMembership() *workspace.Workspace {
	project := func(id, name string) *workspace.Project {
		p := &workspace.Project{ID: id, Path: id[1:], Config: &wsproto.ProjectConfig{Name: name}}
		workspace.ResolveProjectIdentity(p, nil)
		return p
	}
	svc := project("/go/svc", "svc")
	svc.Dependencies = []string{"/go/client", "/go/core", "/go/shared"}
	svc.DependencySources = map[string]wsproto.DependencySource{
		"/go/client": wsproto.DependencySourceModuleGraph,
		"/go/core":   wsproto.DependencySourceGoModule,
	}
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{
		svc, project("/go/client", "client"), project("/go/core", "core"), project("/go/shared", "shared"),
	})
	ws.Name = "ws"
	return ws
}

// A workspace member names the direct dependencies only its module graph
// needs, so the architecture contract can leave them out of the project
// dependencies it observes. The edge stays in dependencies, an import or an
// unattributed entry is no module-graph dependency, and the selection carries
// none.
func TestBuildJobContext_WorkspaceProjectsNameTheirModuleGraphDependencies(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-context-names-member-types", "each-workspace-member-names-its-module-graph-dependencies")
	ws := moduleGraphMembership()
	svc := ws.ProjectByID("/go/svc")
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          svc,
		SelectedProjects: []*workspace.Project{svc},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:           &extension.JobDefinition{Name: "validate-architecture", ExtensionName: "@putnami/sdd"},
	}, nil, nil, nil)

	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("a context naming module-graph dependencies violates the job protocol: %v", diags)
	}
	for _, ref := range parsed.WorkspaceProjects {
		if ref.ID != "/go/svc" {
			if ref.ModuleGraphDependencies != nil {
				t.Errorf("%s moduleGraphDependencies = %q, want none", ref.ID, ref.ModuleGraphDependencies)
			}
			continue
		}
		if want := []string{"/go/client", "/go/core", "/go/shared"}; !slices.Equal(ref.Dependencies, want) {
			t.Errorf("dependencies = %q, want every edge %q", ref.Dependencies, want)
		}
		if want := []string{"/go/client"}; !slices.Equal(ref.ModuleGraphDependencies, want) {
			t.Errorf("moduleGraphDependencies = %q, want %q", ref.ModuleGraphDependencies, want)
		}
	}
	for _, ref := range parsed.SelectedProjects {
		if ref.ModuleGraphDependencies != nil {
			t.Errorf("selectedProjects %s carries moduleGraphDependencies %q", ref.ID, ref.ModuleGraphDependencies)
		}
	}
}
