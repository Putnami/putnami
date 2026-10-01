package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// Two sessions of one workspace, one started through a directory link to its
// root (a symbolic link on Unix, a junction on Windows) and one through the
// root itself, derive the same key ids, so they take the same lock files and
// exclude each other.
func TestTaskOutputLockIdsAgreeAcrossADirectoryLinkedRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := dirlink.Create(root, link); err != nil {
		t.Fatal(err)
	}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/app", Name: "app", Path: "app"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@test/ext"},
	}
	direct := newTaskOutputLocks(root, outputLockHolder{Session: "direct", PID: os.Getpid()}, "", nil)
	linked := newTaskOutputLocks(link, outputLockHolder{Session: "linked", PID: os.Getpid()}, "", nil)
	if direct.root != linked.root {
		t.Fatalf("canonical roots differ: %q through the root, %q through the link", direct.root, linked.root)
	}
	want := direct.keysFor([]*ScheduledJob{job}, nil)
	got := linked.keysFor([]*ScheduledJob{job}, nil)
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("keys through the link = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].id != want[i].id {
			t.Errorf("key %s id = %s through the link, want %s", want[i].name, got[i].id, want[i].id)
		}
	}
}
