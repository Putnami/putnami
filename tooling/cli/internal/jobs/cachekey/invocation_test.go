package cachekey

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestClosureInputsDigest_IsIndependentOfTheCheckoutDirectory pins that a
// closure key input travels: two checkouts of the same content produce the same
// digest, so a remote entry stays shareable between a developer and CI. (The
// other cross-project key mechanism, ExtraFiles, hashes ABSOLUTE paths, which is
// exactly why this digest does not use it.)
func TestClosureInputsDigest_IsIndependentOfTheCheckoutDirectory(t *testing.T) {
	t.Parallel()
	build := func(root string) (*workspace.Workspace, *jobmodel.ScheduledJob) {
		writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2}`)
		writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")
		lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
		app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
		ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
		ws.Graph = workspace.BuildGraph(ws.Projects)
		job := &jobmodel.ScheduledJob{
			Project: app,
			JobDef: &extension.JobDefinition{
				Name: "test~setup",
				TaskCachePolicy: &extension.TaskCachePolicy{
					Key: &extensionproto.TaskCacheKey{ClosureFiles: []string{"infra/requirements.json"}},
				},
			},
		}
		return ws, job
	}

	leftWS, leftJob := build(t.TempDir())
	rightWS, rightJob := build(t.TempDir())
	left, leftErr := closureInputsDigest(leftWS, leftJob, store.NewCacheManager(store.NewLocalStore(t.TempDir())), store.GoEmbedExpander())
	right, rightErr := closureInputsDigest(rightWS, rightJob, store.NewCacheManager(store.NewLocalStore(t.TempDir())), store.GoEmbedExpander())
	if leftErr != nil || rightErr != nil {
		t.Fatalf("closure inputs: %v, %v", leftErr, rightErr)
	}
	if left == "" {
		t.Fatal("a declared closure input produced no digest")
	}
	if left != right {
		t.Errorf("closure digest varies with the checkout directory: %q vs %q", left, right)
	}

	// A task that declares no closure input pays nothing and folds nothing.
	bare := &jobmodel.ScheduledJob{Project: leftJob.Project, JobDef: &extension.JobDefinition{Name: "test~run"}}
	if got, err := closureInputsDigest(leftWS, bare, store.NewCacheManager(store.NewLocalStore(t.TempDir())), store.GoEmbedExpander()); err != nil || got != "" {
		t.Errorf("undeclared closure digest = %q, want empty", got)
	}
}
