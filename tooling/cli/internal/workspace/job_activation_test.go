package workspace

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// A workspace-once command may be planned whatever the project; any other one
// only in a project that uses its extension and holds one of its activation
// files.
func TestMayPlanJob(t *testing.T) {
	found := func([]string) bool { return true }
	missing := func([]string) bool { return false }
	user := &Project{ID: "/libs/app", Name: "app", Extensions: []string{"/go/extension"}}
	other := &Project{ID: "/libs/pub", Name: "pub"}
	command := &extension.JobDefinition{Name: "build", ExtensionName: "@test/go", ExtensionPath: "go/extension", ActivationFiles: []string{"go.mod"}}
	once := &extension.JobDefinition{Name: "validate-workspace", ExtensionName: "@test/go", Activation: workspaceOnceActivation, ActivationFiles: []string{"go.mod"}}
	bare := &extension.JobDefinition{Name: "lint", ExtensionName: "@test/go"}
	for _, tc := range []struct {
		name     string
		project  *Project
		job      *extension.JobDefinition
		hasFiles func([]string) bool
		want     bool
	}{
		{"a user that holds an activation file", user, command, found, true},
		{"a user without an activation file", user, command, missing, false},
		{"a project that does not use the extension", other, command, found, false},
		{"a workspace-once command", other, once, missing, true},
		{"a command without activation files, by extension name", &Project{ID: "/a", Extensions: []string{"@test/go"}}, bare, missing, true},
		{"a command without activation files, unused", other, bare, found, false},
	} {
		if got := MayPlanJob(tc.project, tc.job, tc.hasFiles); got != tc.want {
			t.Errorf("%s: MayPlanJob = %v, want %v", tc.name, got, tc.want)
		}
	}
}
