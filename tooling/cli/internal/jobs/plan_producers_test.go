package jobs

import (
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Plan-time enforcement of typed producer requirements. These fixtures
// replace the config-schema preflight: what used to be
// core parsing a workload's Go imports and stat-ing its infra markers is now a
// declaration the consuming task makes and the planner checks.

func producerWorkspace(t *testing.T, extensionName string) *workspace.Workspace {
	t.Helper()
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "packages/app", Extensions: []string{extensionName}}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{proj})
	ws.Name = "producer-ws"
	return ws
}

// producerPipeline builds a one-command extension whose pipeline is the given
// steps, with the given tasks.
func producerPipeline(name, command string, steps []extension.PipelineStep, tasks map[string]extension.TaskDefinition) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:  name,
		Tasks: tasks,
		Jobs: map[string]*extension.JobDefinition{
			command: {
				ExtensionName: name,
				Name:          command,
				Kind:          "command",
				PipelineSteps: steps,
			},
		},
	}
}

func planProducerCommands(t *testing.T, ws *workspace.Workspace, ext *extension.ExtensionDescription, commands ...string) error {
	t.Helper()
	_, err := Plan(ws, commands, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	return err
}

// TestPlan_RejectsUnboundRequiredTaskInput is the manifest fault: a task states
// it cannot run without another task's output, and the pipeline step never
// names a producer. Both identities must appear — this is the diagnostic that
// replaced "config schema preflight failed for <project>".
func TestPlan_RejectsUnboundRequiredTaskInput(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/deploy")
	ext := producerPipeline("@test/deploy", "deploy",
		[]extension.PipelineStep{{ID: "release", Task: "release-exec"}},
		map[string]extension.TaskDefinition{
			"release-exec": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"schema": {From: extension.TaskInputFromTask},
				},
			},
		})

	err := planProducerCommands(t, ws, ext, "deploy")
	if err == nil {
		t.Fatal("a required task input with no producer planned cleanly")
	}
	for _, want := range []string{
		"required task inputs with no producer",
		"/app:deploy~release",
		"@test/deploy",
		`"release-exec"`,
		`"schema"`,
		"optional",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestPlan_AcceptsBoundRequiredTaskInput is the satisfied case: the consuming
// step binds the port to a producing step that is in the plan.
func TestPlan_AcceptsBoundRequiredTaskInput(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/deploy")
	ext := producerPipeline("@test/deploy", "deploy",
		[]extension.PipelineStep{
			{ID: "extract", Task: "extract-exec"},
			{
				ID:        "release",
				Task:      "release-exec",
				DependsOn: []string{"extract"},
				With: map[string]extension.InputBinding{
					"schema": {FromStep: "extract", Output: "schema"},
				},
			},
		},
		map[string]extension.TaskDefinition{
			"extract-exec": {
				Kind:    "command",
				Command: "true",
				Outputs: map[string]extension.TaskOutputPort{"schema": {Kind: "file", Path: "schema/config.json"}},
			},
			"release-exec": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"schema": {From: extension.TaskInputFromTask},
				},
			},
		})

	if err := planProducerCommands(t, ws, ext, "deploy"); err != nil {
		t.Fatalf("a bound required input must plan cleanly: %v", err)
	}
}

// TestPlan_RejectsRequiredInputWhoseProducerWasPruned is the plan-only fault a
// manifest check cannot see: the producer IS named, but an `if` condition
// spliced it out of this plan, so at run time nothing would write the artifact.
// The diagnostic must name the missing producer step, not just the port.
func TestPlan_RejectsRequiredInputWhoseProducerWasPruned(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/deploy")
	ext := producerPipeline("@test/deploy", "deploy",
		[]extension.PipelineStep{
			{ID: "extract", Task: "extract-exec", If: "params.extract"},
			{
				ID:   "release",
				Task: "release-exec",
				With: map[string]extension.InputBinding{
					"schema": {FromStep: "extract", Output: "schema"},
				},
			},
		},
		map[string]extension.TaskDefinition{
			"extract-exec": {
				Kind:    "command",
				Command: "true",
				Outputs: map[string]extension.TaskOutputPort{"schema": {Kind: "file", Path: "schema/config.json"}},
			},
			"release-exec": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"schema": {From: extension.TaskInputFromTask},
				},
			},
		})

	err := planProducerCommands(t, ws, ext, "deploy")
	if err == nil {
		t.Fatal("a required input whose producer was excluded planned cleanly")
	}
	for _, want := range []string{"/app:deploy~release", `"schema"`, `"extract"`, "not in the plan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestPlan_OptionalTaskInputNeedsNoProducer is the shape @putnami/cloud
// published for C6a: `{"from": "task", "optional": true}` means the consuming
// task handles absence itself. It must plan with no binding at all — this is
// the case the deleted preflight used to fail.
func TestPlan_OptionalTaskInputNeedsNoProducer(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/cloud")
	ext := producerPipeline("@test/cloud", "publish",
		[]extension.PipelineStep{{ID: "publish-config", Task: "cloud-publish-config"}},
		map[string]extension.TaskDefinition{
			"cloud-publish-config": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"schema": {From: extension.TaskInputFromTask, Optional: true},
				},
			},
		})

	if err := planProducerCommands(t, ws, ext, "publish"); err != nil {
		t.Fatalf("an optional task input must plan without a producer: %v", err)
	}
}

// TestPlan_NonTaskInputsAreNotProducerEdges keeps the requirement narrow: a
// file, param, env or runtime input is a cache-key contribution, not a
// producer dependency, and must never make a plan unschedulable.
func TestPlan_NonTaskInputsAreNotProducerEdges(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/lang")
	ext := producerPipeline("@test/lang", "build",
		[]extension.PipelineStep{{ID: "compile", Task: "compile-exec"}},
		map[string]extension.TaskDefinition{
			"compile-exec": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"sources":  {From: extension.TaskInputFromProject, Files: []string{"**/*.go"}},
					"lock":     {From: extension.TaskInputFromWorkspace, Files: []string{"go.work.sum"}},
					"target":   {From: extension.TaskInputFromParams},
					"toolPath": {From: extension.TaskInputFromEnv},
					"platform": {From: extension.TaskInputFromRuntime},
				},
			},
		})

	if err := planProducerCommands(t, ws, ext, "build"); err != nil {
		t.Fatalf("non-task inputs must not require producers: %v", err)
	}
}

// TestPlan_LiteralBindingSatisfiesRequiredInput pins the rule that the
// requirement is "this input has a source", not "a subprocess must run": a
// literal value or a context binding supplies the port with no producer job.
func TestPlan_LiteralBindingSatisfiesRequiredInput(t *testing.T) {
	t.Parallel()
	for name, binding := range map[string]extension.InputBinding{
		"literal value":   {Value: "schema/config.json", HasValue: true},
		"context binding": {From: "project", Path: "path"},
	} {
		ws := producerWorkspace(t, "@test/deploy")
		ext := producerPipeline("@test/deploy", "deploy",
			[]extension.PipelineStep{{
				ID:   "release",
				Task: "release-exec",
				With: map[string]extension.InputBinding{"schema": binding},
			}},
			map[string]extension.TaskDefinition{
				"release-exec": {
					Kind:    "command",
					Command: "true",
					Inputs: map[string]extension.TaskInputPort{
						"schema": {From: extension.TaskInputFromTask},
					},
				},
			})

		if err := planProducerCommands(t, ws, ext, "deploy"); err != nil {
			t.Errorf("%s should satisfy the port: %v", name, err)
		}
	}
}

// TestPlan_ExternalProducerRefIsDeferredToTheDAG keeps one rule for
// cross-project references: resolveExternalDeps expands `^`/`/`/`*` and
// validatePlanDAG proves the result is schedulable. Re-resolving them here with
// a second rule is how two validators come to disagree.
func TestPlan_ExternalProducerRefIsDeferredToTheDAG(t *testing.T) {
	t.Parallel()
	ws := producerWorkspace(t, "@test/deploy")
	ext := producerPipeline("@test/deploy", "deploy",
		[]extension.PipelineStep{{
			ID:   "release",
			Task: "release-exec",
			With: map[string]extension.InputBinding{
				"schema": {FromStep: "^deploy~release", Output: "schema"},
			},
		}},
		map[string]extension.TaskDefinition{
			"release-exec": {
				Kind:    "command",
				Command: "true",
				Inputs: map[string]extension.TaskInputPort{
					"schema": {From: extension.TaskInputFromTask},
				},
			},
		})

	if err := planProducerCommands(t, ws, ext, "deploy"); err != nil {
		t.Fatalf("an upstream-project producer must be left to the DAG check: %v", err)
	}
}

// TestPlan_ProducerDiagnosticsAreDeterministic pins byte-identical diagnostics
// across runs: ports are reported in sorted order, never map-iteration order.
func TestPlan_ProducerDiagnosticsAreDeterministic(t *testing.T) {
	t.Parallel()
	build := func() error {
		ws := producerWorkspace(t, "@test/deploy")
		ext := producerPipeline("@test/deploy", "deploy",
			[]extension.PipelineStep{{ID: "release", Task: "release-exec"}},
			map[string]extension.TaskDefinition{
				"release-exec": {
					Kind:    "command",
					Command: "true",
					Inputs: map[string]extension.TaskInputPort{
						"zeta":     {From: extension.TaskInputFromTask},
						"alpha":    {From: extension.TaskInputFromTask},
						"midpoint": {From: extension.TaskInputFromTask},
					},
				},
			})
		return planProducerCommands(t, ws, ext, "deploy")
	}

	first := build()
	if first == nil {
		t.Fatal("expected a producer error")
	}
	for i := 0; i < 8; i++ {
		if got := build(); got == nil || got.Error() != first.Error() {
			t.Fatalf("diagnostics are not deterministic:\n  %v\n  %v", first, got)
		}
	}
	alpha := strings.Index(first.Error(), `"alpha"`)
	mid := strings.Index(first.Error(), `"midpoint"`)
	zeta := strings.Index(first.Error(), `"zeta"`)
	if !(alpha < mid && mid < zeta) {
		t.Errorf("ports must be reported in sorted order:\n%v", first)
	}
}
