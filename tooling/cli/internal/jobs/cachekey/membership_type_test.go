package cachekey

import (
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The membership a task keyed on the Git candidate cut hashes is the one its
// context carries, so a member that changes only its type moves the key.
func TestWorkspaceMembershipDigestCoversTheResolvedType(t *testing.T) {
	t.Parallel()
	digest := func(libraryType string) string {
		t.Helper()
		library := &workspace.Project{ID: "/go/lib", Path: "go/lib", Config: &wsproto.ProjectConfig{}}
		workspace.ResolveProjectIdentity(library, &wsproto.MergedProject{
			Path: "go/lib", SourceName: "example.com/lib", Type: libraryType,
		})
		ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{library})
		value, err := workspaceMembershipDigest(ws)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	library, application, again := digest("library"), digest("application"), digest("library")
	if library == application {
		t.Error("the membership digest stayed put when a member's type changed")
	}
	if library != again {
		t.Error("the membership digest moved over one membership")
	}
}
