package jobs

import (
	"testing"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
)

// TestTypedIdentity_WorkspaceScopeAndFallbacks covers the two non-plan shapes:
// a workspace-once node reports workspace scope, and a bare result key
// recovers both identity halves.
func TestTypedIdentity_WorkspaceScopeAndFallbacks(t *testing.T) {
	job := &ScheduledJob{
		Project:          &workspace.Project{ID: "ws", Name: "ws"},
		Extension:        &extension.ExtensionDescription{Name: "@x/ext", Version: "1.2.3"},
		JobDef:           &extension.JobDefinition{Name: "install"},
		SelectedProjects: []*workspace.Project{{ID: "/a"}},
	}
	id := job.TypedIdentity()
	if id.Scope != protocolcli.TaskScopeWorkspace {
		t.Errorf("scope = %q, want workspace for a selected-projects node", id.Scope)
	}
	if id.Provider.Extension != "@x/ext" || id.Provider.Version != "1.2.3" {
		t.Errorf("provider = %+v", id.Provider)
	}

	fromKey := TaskIdentityOfKey("/app:build~transpile")
	if fromKey.Project.ID != "/app" || fromKey.Task.Name != "build~transpile" || fromKey.Task.Command != "build" {
		t.Errorf("key fallback = %+v", fromKey)
	}
	if fromKey.Key != "/app:build~transpile" {
		t.Errorf("key fallback did not round-trip: %q", fromKey.Key)
	}
}
