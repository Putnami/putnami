package scheduler

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// runsWithMembership returns a workspace with a TypeScript-style application
// whose runsWith only its provider's view reports (package.json's
// putnami.runsWith; its putnami.json authors none), one whose putnami.json
// authors the list over its provider's, and the provider they run with.
func runsWithMembership() *workspace.Workspace {
	app := &workspace.Project{ID: "/ts/app", Path: "ts/app", Config: &wsproto.ProjectConfig{}}
	workspace.ResolveProjectIdentity(app, &wsproto.MergedProject{
		Path: "ts/app", SourceName: "app", Type: "application", RunsWith: []string{"provider"},
	})
	authored := &workspace.Project{ID: "/ts/authored", Path: "ts/authored",
		Config: &wsproto.ProjectConfig{Name: "authored", RunsWith: []string{"/ts/provider"}}}
	workspace.ResolveProjectIdentity(authored, &wsproto.MergedProject{
		Path: "ts/authored", SourceName: "authored", RunsWith: []string{"elsewhere"},
	})
	provider := &workspace.Project{ID: "/ts/provider", Path: "ts/provider",
		Config: &wsproto.ProjectConfig{Name: "provider", Type: "application"}}
	workspace.ResolveProjectIdentity(provider, nil)
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{app, authored, provider})
	ws.Name = "ws"
	return ws
}

// A task that composes a workload with the workloads it runs with reads each
// member's resolved runsWith from workspaceProjects. The authored config alone
// cannot answer it: a TypeScript project's list can come from its
// package.json through the provider, so its putnami.json authors none. An
// authored list wins over the provider's. The selection and the dependency
// closure carry none.
func TestBuildJobContext_WorkspaceProjectsCarryTheResolvedRunsWith(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-context-names-member-types", "each-workspace-member-names-its-resolved-runs-with")
	ws := runsWithMembership()
	app := ws.ProjectByID("/ts/app")
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          app,
		SelectedProjects: []*workspace.Project{app},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/dev", Path: "/workspace/dev"},
		JobDef:           &extension.JobDefinition{Name: "compose-up", ExtensionName: "@putnami/dev"},
	}, nil, nil, nil)

	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("a context carrying runsWith lists violates the job protocol: %v", diags)
	}
	runsWith := map[string][]string{}
	for _, ref := range parsed.WorkspaceProjects {
		runsWith[ref.ID] = ref.RunsWith
	}
	for id, want := range map[string][]string{
		"/ts/app":      {"provider"},
		"/ts/authored": {"/ts/provider"},
		"/ts/provider": nil,
	} {
		if got := runsWith[id]; !slices.Equal(got, want) {
			t.Errorf("workspaceProjects %s runsWith = %q, want %q", id, got, want)
		}
	}

	for _, list := range []struct {
		name string
		refs any
	}{
		{"selectedProjects", ctx.SelectedProjects},
		{"dependencyClosure", ctx.Project.DependencyClosure},
	} {
		encoded, err := json.Marshal(list.refs)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "runsWith") {
			t.Errorf("%s = %s, want no runsWith", list.name, encoded)
		}
	}
}
