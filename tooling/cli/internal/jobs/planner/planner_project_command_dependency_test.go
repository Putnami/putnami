package planner

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// delegatedStack is a stack whose test and build each run a graph step, which
// waits for the graph step upstream, then the task, as a delegated stack's
// commands do: no manifest dependency orders test after build.
func delegatedStack() *extension.ExtensionDescription {
	command := func(name string) *extension.JobDefinition {
		return &extension.JobDefinition{
			ExtensionName: "@putnami/tool",
			Name:          name,
			PipelineSteps: []extension.PipelineStep{
				{ID: "graph", Task: "graph-run", DependsOn: []string{"^graph"}},
				{ID: name, Task: name + "-run", DependsOn: []string{"graph"}},
			},
		}
	}
	return &extension.ExtensionDescription{
		Name: "@putnami/tool",
		Jobs: map[string]*extension.JobDefinition{"test": command("test"), "build": command("build")},
		Tasks: map[string]extension.TaskDefinition{
			"graph-run": {Kind: "command", Command: "run"},
			"test-run":  {Kind: "command", Command: "run"},
			"build-run": {Kind: "command", Command: "run"},
		},
	}
}

func waitsFor(dependsOn ...string) *wsproto.ProjectConfig {
	return &wsproto.ProjectConfig{Commands: map[string]wsproto.ProjectCommand{"test": {DependsOn: dependsOn}}}
}

// The commands a project's putnami.json names under commands.<command>.dependsOn
// join its own jobs' dependsOn: the planner plans each prerequisite and the
// command's root step waits for its leaves, in the project for `build` and
// across its dependency closure for `^build`. A project that names none plans
// as the manifest says, and a project that runs no build adds no edge.
func TestPlan_ProjectCommandDependencyPlansAndOrdersThePrerequisite(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-project-adds-what-a-command-waits-for", "a-project-command-dependency-plans-and-orders-the-prerequisite")
	tool := []string{"@putnami/tool"}
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: tool, Dependencies: []string{"/lib"}, Config: waitsFor("build", "^build")},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: tool, Dependencies: []string{"/base"}},
		{ID: "/base", Name: "base", Path: "base", Extensions: tool, Config: &wsproto.ProjectConfig{Disable: &wsproto.ProjectDisableConfig{Jobs: []string{"build"}}}},
		{ID: "/plain", Name: "plain", Path: "plain", Extensions: tool, Dependencies: []string{"/lib"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "project-command-dependency"
	exts := []*extension.ExtensionDescription{delegatedStack()}

	planned, err := Plan(ws, []string{"test"}, projects[:1], exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	graph := scheduledJobByKey(planned, "/app:test~graph")
	if graph == nil {
		t.Fatalf("planned = %v, want /app:test~graph", jobKeys(planned))
	}
	for _, want := range []string{"/app:build~build", "/lib:build~build"} {
		if !slices.Contains(graph.DependsOn, want) {
			t.Errorf("/app:test~graph deps = %v, want %s", graph.DependsOn, want)
		}
	}
	for _, key := range jobKeys(planned) {
		if key == "/base:build~build" {
			t.Errorf("planned %s, whose project disables build", key)
		}
	}

	planned, err = Plan(ws, []string{"test"}, projects[3:], exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, key := range jobKeys(planned) {
		if slices.Contains([]string{"/plain:build~build", "/lib:build~build"}, key) {
			t.Errorf("planned %s for a project that names no command dependency: %v", key, jobKeys(planned))
		}
	}

	// The runtime preparation before planning reaches build through the
	// project's entry.
	deps := workspace.ProjectCommandDependencies(ws)
	if want := []string{"^build", "build"}; !slices.Equal(deps["test"], want) {
		t.Fatalf("ProjectCommandDependencies[test] = %v, want %v", deps["test"], want)
	}
	if active := toolchain.CommandDependencyClosureWith(exts, []string{"test"}, deps); !active["build"] {
		t.Errorf("command closure = %v, want build reached through the project's entry", active)
	}
}
