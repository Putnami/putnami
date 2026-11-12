package planner

import (
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cycleTestExtension is a stack shaped like @putnami/pants: test runs a graph
// step after the graph step of every upstream project, then the test step.
// delegated says whether its workspace adapter declares buildGraphEdges.
func cycleTestExtension(name string, delegated bool) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:      name,
		Workspace: &extension.WorkspaceAdapter{Markers: []string{"BUILD"}, DependencySources: true, BuildGraphEdges: delegated},
		Jobs: map[string]*extension.JobDefinition{
			"test": {
				ExtensionName: name,
				Name:          "test",
				PipelineSteps: []extension.PipelineStep{
					{ID: "graph", Task: "graph-run", DependsOn: []string{"^graph"}},
					{ID: "test", Task: "test-run", DependsOn: []string{"graph"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"graph-run": {Kind: "command", Command: "true"},
			"test-run":  {Kind: "command", Command: "true"},
		},
	}
}

// cycleWorkspace is buildbuddy's shape: /proto and /proto/api/v1 depend on
// each other, /proto depends on /third outside the cycle, and /server depends
// on /proto/api/v1.
func cycleWorkspace(cycleExtensions ...string) *workspace.Workspace {
	delegated := []string{"@fixture/delegated"}
	projects := []*workspace.Project{
		{ID: "/third", Name: "third", Path: "third", Extensions: delegated},
		{ID: "/proto", Name: "proto", Path: "proto", Extensions: cycleExtensions, Dependencies: []string{"/proto/api/v1", "/third"}},
		{ID: "/proto/api/v1", Name: "v1", Path: "proto/api/v1", Extensions: delegated, Dependencies: []string{"/proto"}},
		{ID: "/server", Name: "server", Path: "server", Extensions: delegated, Dependencies: []string{"/proto/api/v1"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "cycle-ws"
	return ws
}

// runOrder runs the plan through the scheduler's DAG state, one ready job at
// a time in key order, and returns the keys in the order they ran. It fails
// when a job never becomes ready.
func runOrder(t *testing.T, planned []*jobmodel.ScheduledJob) []string {
	t.Helper()
	state := NewDAGState(planned)
	var order []string
	ready := state.Ready()
	for len(ready) > 0 {
		slices.SortFunc(ready, func(a, b *jobmodel.ScheduledJob) int { return strings.Compare(a.Key(), b.Key()) })
		next := ready[0]
		order = append(order, next.Key())
		ready = append(ready[1:], state.Complete(next.Key())...)
	}
	if len(order) != len(planned) {
		t.Fatalf("ran %d of %d jobs: %v", len(order), len(planned), order)
	}
	return order
}

func TestPlan_ADelegatedCycleRunsAsOneComponent(t *testing.T) {
	t.Parallel()
	ws := cycleWorkspace("@fixture/delegated")
	planned, err := Plan(ws, []string{"test"}, ws.Projects,
		[]*extension.ExtensionDescription{cycleTestExtension("@fixture/delegated", true)}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// No member of the cycle waits for the other; both wait for what either
	// depends on outside it.
	for key, want := range map[string][]string{
		"/third:test~graph":        nil,
		"/proto:test~graph":        {"/third:test~graph"},
		"/proto/api/v1:test~graph": {"/third:test~graph"},
		"/server:test~graph":       {"/proto/api/v1:test~graph"},
	} {
		job := scheduledJobByKey(planned, key)
		if job == nil {
			t.Fatalf("%s is not planned: %v", key, jobKeys(planned))
		}
		if !slices.Equal(job.DependsOn, want) {
			t.Errorf("%s depends on %v, want %v", key, job.DependsOn, want)
		}
	}

	order := runOrder(t, planned)
	position := make(map[string]int, len(order))
	for i, key := range order {
		position[key] = i
	}
	for _, job := range planned {
		for _, dep := range job.DependsOn {
			if position[dep] > position[job.Key()] {
				t.Errorf("%s ran before its dependency %s: %v", job.Key(), dep, order)
			}
		}
	}
	for _, member := range []string{"/proto:test~graph", "/proto/api/v1:test~graph"} {
		if position[member] < position["/third:test~graph"] || position[member] > position["/server:test~graph"] {
			t.Errorf("%s ran outside the component's place between /third and /server: %v", member, order)
		}
	}
}

// Selecting one member of the cycle plans the upstream graph step of what the
// component depends on, and none of its peers'.
func TestPlan_AMemberOfADelegatedCycleWaitsForTheComponentsUpstream(t *testing.T) {
	t.Parallel()
	ws := cycleWorkspace("@fixture/delegated")
	planned, err := Plan(ws, []string{"test"}, []*workspace.Project{ws.ProjectByID("/proto/api/v1")},
		[]*extension.ExtensionDescription{cycleTestExtension("@fixture/delegated", true)}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := jobKeys(planned)
	slices.Sort(got)
	want := []string{"/proto/api/v1:test~graph", "/proto/api/v1:test~test", "/third:test~graph"}
	if !slices.Equal(got, want) {
		t.Errorf("planned = %v, want %v", got, want)
	}
}

func TestPlan_ACycleWithATaskPutnamiOrdersFails(t *testing.T) {
	t.Parallel()
	ws := cycleWorkspace("@fixture/delegated", "@fixture/ordered")
	_, err := Plan(ws, []string{"test"}, ws.Projects, []*extension.ExtensionDescription{
		cycleTestExtension("@fixture/delegated", true),
		cycleTestExtension("@fixture/ordered", false),
	}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("a plan ran a task Putnami orders inside a dependency cycle")
	}
	for _, want := range []string{"dependency cycle among /proto, /proto/api/v1", "/proto runs", "@fixture/ordered", "workspace.buildGraphEdges"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
