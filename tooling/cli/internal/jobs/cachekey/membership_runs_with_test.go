package cachekey

import (
	"encoding/json"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// providerRunsWithWorkspace returns a workspace whose /ts/app declares no
// runsWith in putnami.json and runs with what its provider's view reports.
func providerRunsWithWorkspace(runsWith []string) *workspace.Workspace {
	app := &workspace.Project{ID: "/ts/app", Path: "ts/app", Config: &wsproto.ProjectConfig{}}
	workspace.ResolveProjectIdentity(app, &wsproto.MergedProject{
		Path: "ts/app", SourceName: "app", Type: "application", RunsWith: runsWith,
	})
	return workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{app})
}

// The membership a task keyed on the Git candidate cut hashes is the one its
// context carries, so a member whose provider changes only its runsWith list
// moves the key.
func TestWorkspaceMembershipDigestCoversTheResolvedRunsWith(t *testing.T) {
	t.Parallel()
	digest := func(runsWith []string) string {
		t.Helper()
		value, err := workspaceMembershipDigest(providerRunsWithWorkspace(runsWith))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	provider, none, again := digest([]string{"provider"}), digest(nil), digest([]string{"provider"})
	if provider == none {
		t.Error("the membership digest stayed put when a member's runsWith changed")
	}
	if provider != again {
		t.Error("the membership digest moved over one membership")
	}
}

// A workspaceProjects entry carries the runsWith list the provider's view
// reports when putnami.json authors none, and an entry that runs with nothing
// encodes no runsWith member, so a membership without runsWith hashes the
// bytes it hashed before the member existed.
func TestWorkspaceProjectsContextCarriesTheResolvedRunsWith(t *testing.T) {
	t.Parallel()
	members := WorkspaceProjectsContext(providerRunsWithWorkspace([]string{"provider"}), nil)
	if len(members) != 1 || strings.Join(members[0].RunsWith, ",") != "provider" {
		t.Fatalf("workspaceProjects = %+v, want /ts/app running with provider", members)
	}
	encoded, err := json.Marshal(WorkspaceProjectsContext(providerRunsWithWorkspace(nil), nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "runsWith") {
		t.Errorf("workspaceProjects = %s, want runsWith omitted for a member that runs with nothing", encoded)
	}
}
