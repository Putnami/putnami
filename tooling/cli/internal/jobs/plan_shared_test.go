package jobs

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The plan half of deciding which nodes are ONE shared execution.
//
// The fixture mirrors the shape the Go extension actually ships, because that
// shape is the whole decision: `build` and `test` schedule the same
// `probe-describe` and `probe-generate` tasks with byte-identical definitions,
// while generate owns the project `gen` subtree. An earlier audit found the
// first pair is safe to unify and the second is not until the TypeScript
// capability-manifest clobber is fixed, so the tests below pin exactly that
// split rather than the mechanism in the abstract. A focused variant adds the
// now-live test-mode literal to pin producer-chain identity.

func sharedProbeExtension() *extension.ExtensionDescription {
	enabled := true
	describeTask := extension.TaskDefinition{
		Kind:    "command",
		Command: "probe",
		Args:    []string{"describe"},
		Cwd:     "{projectRoot}",
		Inputs: map[string]extension.TaskInputPort{
			"sources": {From: "project", Files: []string{"**/*.go"}},
		},
		Writes: []extension.ResourceRef{{ID: "clients"}},
		Reads:  []extension.ResourceRef{{ID: "gen"}},
		Cache:  &extension.TaskCachePolicy{Enabled: &enabled, Deterministic: true},
		Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"client": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "clients"},
			},
		},
	}
	generateTask := extension.TaskDefinition{
		Kind:    "command",
		Command: "probe",
		Args:    []string{"generate"},
		Cwd:     "{projectRoot}",
		Inputs: map[string]extension.TaskInputPort{
			"sources": {From: "project", Files: []string{"**/*.go"}},
			"mode":    {From: "params"},
		},
		Writes: []extension.ResourceRef{{ID: genResourceID}},
		Cache:  &extension.TaskCachePolicy{Enabled: &enabled, Deterministic: true},
		Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"gen": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"},
			},
		},
	}
	return &extension.ExtensionDescription{
		Name:    "@test/probe",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/probe",
				Name:          "build",
				Kind:          "command",
				Command:       "probe",
				Cache:         true,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "probe-generate"},
					{ID: "describe", Task: "probe-describe", DependsOn: []string{"generate"}},
				},
			},
			"test": {
				ExtensionName: "@test/probe",
				Name:          "test",
				Kind:          "command",
				Command:       "probe",
				Cache:         true,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "probe-generate"},
					{ID: "describe", Task: "probe-describe", DependsOn: []string{"generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"probe-describe": describeTask,
			"probe-generate": generateTask,
		},
	}
}

func sharedProbeExtensionWithTestMode() *extension.ExtensionDescription {
	ext := sharedProbeExtension()
	testCommand := *ext.Jobs["test"]
	testCommand.PipelineSteps = append([]extension.PipelineStep(nil), testCommand.PipelineSteps...)
	testCommand.PipelineSteps[0].With = map[string]extension.InputBinding{
		"mode": {Value: "test", HasValue: true},
	}
	ext.Jobs["test"] = &testCommand
	return ext
}

func sharedProbeWorkspace(root string) *workspace.Workspace {
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: []string{"@test/probe"}},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "probe-ws"
	return ws
}

// planSharedProbe plans `build,test` over the probe fixture.
func planSharedProbe(t *testing.T, root string) []*ScheduledJob {
	t.Helper()
	return planSharedProbeWithExtension(t, root, sharedProbeExtension())
}

func planSharedProbeWithExtension(
	t *testing.T,
	root string,
	ext *extension.ExtensionDescription,
) []*ScheduledJob {
	t.Helper()
	ws := sharedProbeWorkspace(root)
	planned, err := Plan(ws, []string{"build", "test"}, ws.Projects,
		[]*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

func plannedByName(planned []*ScheduledJob) map[string]*ScheduledJob {
	byName := make(map[string]*ScheduledJob, len(planned))
	for _, job := range planned {
		byName[job.DisplayName()] = job
	}
	return byName
}

// TestSharedExecutions_UnifyDescribeAcrossCommands is the slice's payload: two
// commands scheduling one manifest task over one project with identical
// declared inputs are ONE physical execution, tagged as such at plan time.
func TestSharedExecutions_UnifyDescribeAcrossCommands(t *testing.T) {
	t.Parallel()
	planned := planSharedProbe(t, t.TempDir())
	byName := plannedByName(planned)

	buildDescribe := byName["build~describe"]
	testDescribe := byName["test~describe"]
	if buildDescribe == nil || testDescribe == nil {
		t.Fatalf("fixture did not plan both describe nodes: %v", plannedNames(planned))
	}
	if buildDescribe.SharedExecution() == "" {
		t.Fatal("build~describe carries no shared execution id")
	}
	if buildDescribe.SharedExecution() != testDescribe.SharedExecution() {
		t.Errorf("describe nodes belong to different shared executions: %q vs %q",
			buildDescribe.SharedExecution(), testDescribe.SharedExecution())
	}
}

func TestSharedExecutions_NeverMixCapabilityAndOrdinaryJobsInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		name := "ordinary-first"
		if reverse {
			name = "protected-first"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			planned := planSharedProbe(t, root)
			byName := plannedByName(planned)
			protected := byName["build~describe"]
			ordinary := byName["test~describe"]
			if protected == nil || ordinary == nil {
				t.Fatalf("fixture did not plan both describe nodes: %v", plannedNames(planned))
			}
			protected.JobDef.Traits.SideEffects = extensionproto.SideEffectsCloud
			for _, job := range planned {
				job.SharedExecutionID = ""
			}
			if reverse {
				for left, right := 0, len(planned)-1; left < right; left, right = left+1, right-1 {
					planned[left], planned[right] = planned[right], planned[left]
				}
			}
			attachSharedExecutions(sharedProbeWorkspace(root), planned, nil)
			if protected.SharedExecution() != "" || ordinary.SharedExecution() != "" {
				t.Fatalf("cross-regime jobs shared execution %q/%q", protected.SharedExecution(), ordinary.SharedExecution())
			}
		})
	}
}

// TestSharedExecutions_SeparateDescribeAfterDifferentLiteralProducers pins the
// safety boundary. Once test~generate receives mode=test, a downstream
// describe cannot coalesce with build~describe merely because the describe
// steps themselves are identical: their functional producers are different
// work. The recursive identity must also be byte-stable across Plan calls.
func TestSharedExecutions_SeparateDescribeAfterDifferentLiteralProducers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plan := func() []*ScheduledJob {
		return planSharedProbeWithExtension(t, root, sharedProbeExtensionWithTestMode())
	}

	identityMap := func(planned []*ScheduledJob) map[string]string {
		t.Helper()
		ws := sharedProbeWorkspace(root)
		resolver := newSharedProducerIdentityResolver(ws, planned, nil)
		identities := make(map[string]string)
		for _, job := range planned {
			if job.StepID() != "describe" {
				continue
			}
			identity, ok := sharedExecutionIdentity(ws, job, nil, resolver)
			if !ok {
				t.Fatalf("%s did not produce a shared-execution identity", job.DisplayName())
			}
			identities[job.DisplayName()] = identity
			if got := job.SharedExecution(); got != "" {
				t.Errorf("%s joined shared execution %q despite a distinct producer", job.DisplayName(), got)
			}
		}
		return identities
	}

	first := identityMap(plan())
	second := identityMap(plan())
	if first["build~describe"] == first["test~describe"] {
		t.Fatal("describe nodes with build/test literal producer inputs have the same identity")
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("producer-aware identities are not deterministic across Plan calls:\n%v\n%v", first, second)
	}
}

func TestSharedExecutions_ProducerIdentityFailsClosedOnCycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	planned := planSharedProbe(t, root)
	byName := plannedByName(planned)
	buildGenerate := byName["build~generate"]
	testGenerate := byName["test~generate"]
	if buildGenerate == nil || testGenerate == nil {
		t.Fatalf("fixture did not plan both producers: %v", plannedNames(planned))
	}

	buildGenerate.DependsOn = []string{testGenerate.Key()}
	testGenerate.DependsOn = []string{buildGenerate.Key()}
	for _, job := range planned {
		job.SharedExecutionID = ""
	}
	attachSharedExecutions(sharedProbeWorkspace(root), planned, nil)

	for _, name := range []string{"build~describe", "test~describe"} {
		if got := byName[name].SharedExecution(); got != "" {
			t.Errorf("%s joined %q through a cyclic producer identity", name, got)
		}
	}
}

// TestSharedExecutions_LeaveGenerateAlone pins the W2a audit's blocker. The
// project `gen` subtree is written by tasks that do not own it — on TypeScript
// applications `test~test` rewrites `.gen/schema/capabilities.json` after
// generate finished, and deletes it when the suite fails — so a shared node
// there would publish a capture of somebody else's writes. It stays split until
// that ownership violation is fixed.
func TestSharedExecutions_LeaveGenerateAlone(t *testing.T) {
	t.Parallel()
	planned := planSharedProbe(t, t.TempDir())
	for _, job := range planned {
		if job.StepID() != "generate" {
			continue
		}
		if id := job.SharedExecution(); id != "" {
			t.Errorf("%s was unified into %q; generate must stay split", job.DisplayName(), id)
		}
	}
}

// TestSharedExecutions_RequireDeclaredWritesAndDeterminism keeps the mechanism
// off tasks that cannot support the claim it makes on their behalf.
func TestSharedExecutions_RequireDeclaredWritesAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws := sharedProbeWorkspace(root)

	for _, tc := range []struct {
		name   string
		mutate func(task *extension.TaskDefinition)
	}{
		{"not deterministic", func(task *extension.TaskDefinition) { task.Cache.Deterministic = false }},
		{"no declared writes", func(task *extension.TaskDefinition) { task.Writes = nil }},
		{"no declared inputs", func(task *extension.TaskDefinition) { task.Inputs = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := sharedProbeExtension()
			task := ext.Tasks["probe-describe"]
			tc.mutate(&task)
			ext.Tasks["probe-describe"] = task

			planned, err := Plan(ws, []string{"build", "test"}, ws.Projects,
				[]*extension.ExtensionDescription{ext}, nil, nil, nil)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			for _, job := range planned {
				if job.StepID() == "describe" && job.SharedExecution() != "" {
					t.Errorf("%s was unified despite %s", job.DisplayName(), tc.name)
				}
			}
		})
	}
}

// TestSharedExecutions_SeparateDifferentProjects keeps the grouping keyed on the
// project: two projects running the same task are different work.
func TestSharedExecutions_SeparateDifferentProjects(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: []string{"@test/probe"}},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/probe"}},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "probe-ws"

	planned, err := Plan(ws, []string{"build", "test"}, ws.Projects,
		[]*extension.ExtensionDescription{sharedProbeExtension()}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	byProject := map[string]string{}
	for _, job := range planned {
		if job.StepID() != "describe" {
			continue
		}
		id := job.SharedExecution()
		if id == "" {
			t.Fatalf("%s %s carries no shared execution id", job.Project.ID, job.DisplayName())
		}
		if seen, ok := byProject[job.Project.ID]; ok && seen != id {
			t.Errorf("project %s describe nodes disagree: %q vs %q", job.Project.ID, seen, id)
		}
		byProject[job.Project.ID] = id
	}
	if len(byProject) != 2 {
		t.Fatalf("expected a shared node per project, got %v", byProject)
	}
	if byProject["/app"] == byProject["/lib"] {
		t.Errorf("two projects share one execution id %q", byProject["/app"])
	}
}

// TestSharedExecutions_DoNotMoveThePlan is acceptance criterion 3, at the plan
// level: unification adds NO edge and changes NO cache key. The shared id is an
// execution grouping and nothing derived from it may reach an address.
//
// Clearing the stamp reconstructs the plan exactly as it was before this slice,
// which is what makes the comparison a regression test rather than a tautology:
// if anything else in Plan had started reading the field, the two sides would
// diverge here.
func TestSharedExecutions_DoNotMoveThePlan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "app", "main.go", "package main\n")
	ws := sharedProbeWorkspace(root)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))

	planned := planSharedProbe(t, root)
	shared := 0
	withSharing := map[string]string{}
	edges := map[string][]string{}
	for _, job := range planned {
		if job.SharedExecution() != "" {
			shared++
		}
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, map[string]string{})
		if err != nil {
			t.Fatalf("cache hash for %s: %v", job.Key(), err)
		}
		withSharing[job.Key()] = hash
		edges[job.Key()] = append(append([]string(nil), job.DependsOn...), job.SerializeAfter...)
	}
	if shared == 0 {
		t.Fatal("fixture produced no shared node, so the comparison proves nothing")
	}

	for _, job := range planned {
		job.SharedExecutionID = ""
	}
	for _, job := range planned {
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, map[string]string{})
		if err != nil {
			t.Fatalf("cache hash for %s: %v", job.Key(), err)
		}
		if hash != withSharing[job.Key()] {
			t.Errorf("%s cache key moved with sharing: %s != %s", job.Key(), withSharing[job.Key()], hash)
		}
		got := append(append([]string(nil), job.DependsOn...), job.SerializeAfter...)
		if !reflect.DeepEqual(got, edges[job.Key()]) {
			t.Errorf("%s edges changed with sharing: %v != %v", job.Key(), edges[job.Key()], got)
		}
	}
}

// TestSharedExecutions_AreDeterministic pins that two Plan calls over one
// workspace hand the same nodes the same ids, so a plan preview, a change plan
// and a run all describe the same grouping.
func TestSharedExecutions_AreDeterministic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := sharedExecutionMap(planSharedProbe(t, root))
	second := sharedExecutionMap(planSharedProbe(t, root))
	if !reflect.DeepEqual(first, second) {
		t.Errorf("shared execution ids are not stable across Plan calls:\n%v\n%v", first, second)
	}
}

func sharedExecutionMap(planned []*ScheduledJob) map[string]string {
	out := map[string]string{}
	for _, job := range planned {
		if id := job.SharedExecution(); id != "" {
			out[job.Key()] = id
		}
	}
	return out
}

func plannedNames(planned []*ScheduledJob) []string {
	names := make([]string, 0, len(planned))
	for _, job := range planned {
		names = append(names, job.DisplayName())
	}
	sort.Strings(names)
	return names
}
