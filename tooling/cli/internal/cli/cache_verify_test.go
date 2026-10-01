package cli

import (
	"context"
	"io"
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestCacheVerifyRunnerRoutesExecutionThroughEngine(t *testing.T) {
	t.Parallel()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	job := &jobs.ScheduledJob{Project: project}
	var requests []engine.Request
	runner := newCacheVerifyRunner(nil, GlobalFlags{},
		func(_ context.Context, request engine.Request, sink engine.EventSink) (engine.SessionResult, error) {
			if sink == nil {
				t.Fatal("cache verify engine renderer is nil")
			}
			requests = append(requests, request)
			return engine.SessionResult{ExitCode: engine.ExitSuccess, Projects: []*workspace.Project{project},
				Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{}}, nil
		})

	if _, err := runner(context.Background(), "/staged", "/isolated-store"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	for _, request := range requests {
		if request.WorkspaceRoot != "/staged" || !reflect.DeepEqual(request.Commands, []string{"lint", "test", "build"}) {
			t.Fatalf("request = %#v, want staged canonical command run", request)
		}
		if !request.Global.All || request.Global.NoCache || request.Global.MaxParallel != 1 ||
			!request.Global.Quiet || !request.EphemeralSession || request.Stdout != io.Discard {
			t.Fatalf("request policy = %#v", request)
		}
	}
	if requests[0].Global.Plan || requests[0].CacheVerification == nil ||
		requests[0].CacheVerification.StoreRoot != "/isolated-store" {
		t.Fatalf("execution request = %#v, want isolated cache verification", requests[0])
	}
}
