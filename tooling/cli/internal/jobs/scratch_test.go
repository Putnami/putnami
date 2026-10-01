package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestJobInvocationSharesScratchLeaseAndCancelsConcurrently(t *testing.T) {
	root := t.TempDir()
	ws := &workspace.Workspace{Root: root, Name: "scratch"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/p", Name: "p", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "scratch", Path: root},
		JobDef:    &extension.JobDefinition{Name: "build", Command: "/bin/true"},
	}
	ctx, cancel, inv, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()
	cmd := inv.execCommand(ctx)
	if flock.Inheritable && (len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != inv.scratchLease.File()) {
		t.Fatal("job child did not inherit its context's scratch lease")
	}
	if !flock.Inheritable && len(cmd.ExtraFiles) != 0 {
		t.Fatal("job child was given descriptors this platform cannot pass")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(cancel)
	}
	wg.Wait()
	lock, err := flock.Acquire(filepath.Join(root, ".putnami", "cache.lock"), true, true)
	if err != nil {
		t.Fatalf("invocation cleanup leaked its lease: %v", err)
	}
	_ = lock.Close()
}
