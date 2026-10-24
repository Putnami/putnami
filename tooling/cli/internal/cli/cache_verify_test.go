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
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
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
		}, nil)

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
			!request.Global.Quiet || !request.EphemeralSession || request.Stdout != io.Discard || request.ErrorOut != io.Discard {
			t.Fatalf("request policy = %#v", request)
		}
	}
	if requests[0].Global.Plan || requests[0].CacheVerification == nil ||
		requests[0].CacheVerification.StoreRoot != "/isolated-store" {
		t.Fatalf("execution request = %#v, want isolated cache verification", requests[0])
	}
}

// TestCacheVerifyRunnerKeepsTheWarningsOfTheFirstRunOnly holds the runner to
// one copy of the workspace warnings: the three runs load the same HEAD, so
// each warning appears once in the cache verify envelope.
func TestCacheVerifyRunnerKeepsTheWarningsOfTheFirstRunOnly(t *testing.T) {
	t.Parallel()
	warning := workspace.Warning{Code: workspace.WarningCodeJobsIgnored, Message: "project /app declares jobs"}
	report := workspace.NewWarningReport(nil)
	runner := newCacheVerifyRunner(nil, GlobalFlags{}, []string{"lint"},
		func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error) {
			return engine.SessionResult{ExitCode: engine.ExitSuccess, WorkspaceWarnings: []workspace.Warning{warning}}, nil
		}, report)
	for _, label := range []string{"first live run", "second live run", "cache-hit run"} {
		if _, err := runner(context.Background(), "/staged", "/isolated-store"); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	want := []protocolcli.WorkspaceWarning{{Code: string(warning.Code), Message: warning.Message}}
	if got := report.Envelope(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the report holds %+v, want the first run's warning once: %+v", got, want)
	}
}

// TestCacheVerifyRunsTheWorkspaceGate pins that cache verify runs the
// workspace's gate in the order of its CI document, and no command list of the
// core's own. Without a usable document it runs the blocking commands of the
// protocol's default CI document.
func TestCacheVerifyRunsTheWorkspaceGate(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "cache-verify-runs-the-gate")
	t.Parallel()
	bare := t.TempDir()
	var defaults []string
	for _, command := range ciproto.DefaultDocument().Commands {
		if command.Blocking() {
			defaults = append(defaults, command.Name)
		}
	}
	if got := cacheVerifyCommands(bare); !reflect.DeepEqual(got, defaults) {
		t.Errorf("commands without a CI document = %v, want the default document's %v", got, defaults)
	}
	gated := t.TempDir()
	if err := os.WriteFile(filepath.Join(gated, ciproto.Filename), []byte(`{"version":3,"commands":["test","check",{"name":"audit","failOnError":false},"lint"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := cacheVerifyCommands(gated), []string{"test", "check", "lint"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want the CI document's blocking commands in its order %v", got, want)
	}
	if got, want := cacheVerifyCommands(gated), strings.Split(agentctx.GateTasks(gated), ","); !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want the generated guidance gate %v", got, want)
	}
}
