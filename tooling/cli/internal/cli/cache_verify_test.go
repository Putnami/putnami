package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestCacheVerifyRunnerRoutesExecutionThroughEngine(t *testing.T) {
	t.Parallel()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	job := &jobs.ScheduledJob{Project: project}
	var requests []engine.Request
	commands := []string{"lint", "test", "build", "validate"}
	runner := newCacheVerifyRunner(nil, GlobalFlags{}, commands,
		func(_ context.Context, request engine.Request, sink engine.EventSink) (engine.SessionResult, error) {
			if sink == nil {
				t.Fatal("cache verify engine renderer is nil")
			}
			requests = append(requests, request)
			return engine.SessionResult{ExitCode: engine.ExitSuccess, Projects: []*workspace.Project{project},
				Plan: []*jobs.ScheduledJob{job}, Results: map[string]*jobs.JobResult{}}, nil
		})

	// The runner keeps its own copy of the commands it was built with.
	commands[0] = "format"
	if _, err := runner(context.Background(), "/staged", "/isolated-store"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	for _, request := range requests {
		if request.WorkspaceRoot != "/staged" || !reflect.DeepEqual(request.Commands, []string{"lint", "test", "build", "validate"}) {
			t.Fatalf("request = %#v, want a staged run of the commands the runner was built with", request)
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

// TestCacheVerifyRunsTheWorkspaceGate pins that cache verify runs the
// workspace's gate, read from its CI document, and no command list of the
// core's own.
func TestCacheVerifyRunsTheWorkspaceGate(t *testing.T) {
	t.Parallel()
	bare := t.TempDir()
	if got, want := cacheVerifyCommands(bare), strings.Split(agentctx.GateTasks(bare), ","); !reflect.DeepEqual(got, want) {
		t.Errorf("commands without a CI document = %v, want the gate %v", got, want)
	}
	gated := t.TempDir()
	if err := os.WriteFile(filepath.Join(gated, ciproto.Filename), []byte(`{"version":3,"commands":["check","lint"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := cacheVerifyCommands(gated), []string{"lint", "check"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want the CI document's gate %v", got, want)
	}
}
