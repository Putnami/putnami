package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestChangePlanPlannerRoutesThroughEngine(t *testing.T) {
	t.Parallel()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	jobs := []*jobs.ScheduledJob{{Project: project}}
	planner := newChangePlanPlanner("/workspace", &wsproto.Config{},
		func(_ context.Context, request engine.Request, sink engine.EventSink) (engine.SessionResult, error) {
			if sink != nil {
				t.Fatalf("event sink = %T, want nil", sink)
			}
			// The plan must project the SAME gate CI runs. A command CI runs
			// and the plan omits is a task nobody reviewed.
			if request.WorkspaceRoot != "/workspace" || !reflect.DeepEqual(request.Commands, changePlanCommands) {
				t.Fatalf("request = %#v, want the change-plan workspace and commands %v", request, changePlanCommands)
			}
			if !request.Global.Impacted || request.Global.Baseline != "0123456789012345678901234567890123456789" || !request.Global.NoCache || !request.Global.Plan || !request.Global.Quiet {
				t.Fatalf("global = %#v, want immutable impacted plan flags", request.Global)
			}
			return engine.SessionResult{ExitCode: engine.ExitSuccess, Projects: []*workspace.Project{project}, Plan: jobs}, nil
		})

	result, err := planner(context.Background(), "0123456789012345678901234567890123456789", true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !result.Complete || !reflect.DeepEqual(result.Projects, []*workspace.Project{project}) || !reflect.DeepEqual(result.Jobs, jobs) {
		t.Fatalf("result = %#v, want complete engine result", result)
	}
	if !reflect.DeepEqual(result.Commands, changePlanCommands) {
		t.Fatalf("result commands = %v, want the planned set %v", result.Commands, changePlanCommands)
	}
}

// TestChangePlanPlannerErrorNamesThePlannedCommands holds the planning error to
// changePlanCommands: a diagnostic that names fewer commands than the planner
// ran misstates what failed.
func TestChangePlanPlannerErrorNamesThePlannedCommands(t *testing.T) {
	t.Parallel()
	engineErr := errors.New("engine refused the plan")
	planner := newChangePlanPlanner("/workspace", &wsproto.Config{},
		func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error) {
			return engine.SessionResult{}, engineErr
		})
	_, err := planner(context.Background(), "0123456789012345678901234567890123456789", false)
	want := "plan " + strings.Join(changePlanCommands, ",") + ": engine refused the plan"
	if err == nil || err.Error() != want || !errors.Is(err, engineErr) {
		t.Fatalf("error = %v, want %q wrapping the engine error", err, want)
	}
}

// TestChangePlanDocNamesTheChangePlanCommands is the drift guard between
// changePlanCommands and doc 17, the page every change-plan consumer reads.
// Every comma-separated command list the page spells must be exactly the set
// the planner runs, in its order.
func TestChangePlanDocNamesTheChangePlanCommands(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "doc", "17-ci-change-plans.md"))
	if err != nil {
		t.Fatalf("read doc 17: %v", err)
	}
	lists := regexp.MustCompile("`(lint(?:,[a-z-]+)+)`").FindAllStringSubmatch(string(doc), -1)
	if len(lists) == 0 {
		t.Fatal("doc 17 names no command set; it must state the one change-plan plans")
	}
	want := strings.Join(changePlanCommands, ",")
	for _, list := range lists {
		if list[1] != want {
			t.Errorf("doc 17 names the command set %q, change-plan plans %q", list[1], want)
		}
	}
}

// TestChangePlanCommandsMatchTheCIQualityJob is the drift guard between the two
// declarations of one gate.
//
// The generated guidance says what CI runs for this workspace — derived from
// its CI document when one exists, otherwise from its SDD extension
// declaration, which is the fixed gate Putnami Cloud's native runner executes.
// changePlanCommands says what `putnami ci change-plan` shows a reviewer it
// will run. A command in one and not the other is either a check nobody
// reviewed or a review of a check nobody performs, and both are silent — the
// plan renders fine either way.
func TestChangePlanCommandsMatchTheCIQualityJob(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the workspace root: %v", err)
	}
	gate := agentctx.GateTasks(root)
	if gate == "lint,test,build" {
		t.Fatal("the workspace derives only the generic gate; it declares @putnami/sdd and the comparison would be vacuous")
	}
	want := append([]string(nil), changePlanCommands...)
	got := strings.Split(gate, ",")
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace gate = %q, change-plan commands = %v; the two declarations of one gate disagree",
			gate, changePlanCommands)
	}
}
