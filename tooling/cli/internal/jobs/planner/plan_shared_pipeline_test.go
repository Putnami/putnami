package planner

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Shared executions over the build and test pipelines.
//
// The fixture mirrors the Go `build` and `test` pipelines, because their shape
// is the decision: build's generate follows a tidy, test's generate follows a
// config merge, and neither producer declares a write. Describe and the
// compile and test steps follow generate and read the gen tree it writes.

func pipelineProbeExtension() *extension.ExtensionDescription {
	enabled := true
	deterministic := func() *extension.TaskCachePolicy {
		return &extension.TaskCachePolicy{Enabled: &enabled, Deterministic: true}
	}
	inputs := func(params ...string) map[string]extension.TaskInputPort {
		ports := map[string]extension.TaskInputPort{
			"sources": {From: "project", Files: []string{"**/*.go", "!**/*_test.go"}},
		}
		for _, param := range params {
			ports[param] = extension.TaskInputPort{From: "params"}
		}
		return ports
	}
	configMerge := func(file string) extension.TaskDefinition {
		return extension.TaskDefinition{
			Kind: "command", Command: "probe", Args: []string{"config-merge", file}, Cwd: "{projectRoot}",
			Inputs: map[string]extension.TaskInputPort{"config": {From: "project", Files: []string{"putnami.json"}}},
			Cache:  deterministic(),
			Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"env": {
					Kind: extension.OutputKindFile, Root: extension.OutputRootProject,
					Path: ".gen/conf/" + file, OptionalEmpty: true,
				},
			}},
		}
	}
	task := func(args string, writes, reads []string, declares *extension.TaskDeclaration, params ...string) extension.TaskDefinition {
		def := extension.TaskDefinition{
			Kind: "command", Command: "probe", Args: []string{args}, Cwd: "{projectRoot}",
			Inputs:   inputs(params...),
			Cache:    deterministic(),
			Declares: declares,
		}
		for _, id := range writes {
			def.Writes = append(def.Writes, extension.ResourceRef{ID: id})
		}
		for _, id := range reads {
			def.Reads = append(def.Reads, extension.ResourceRef{ID: id})
		}
		return def
	}
	tidy := extension.TaskDefinition{
		Kind: "command", Command: "probe", Args: []string{"tidy"}, Cwd: "{projectRoot}",
		Inputs: map[string]extension.TaskInputPort{"modules": {From: "project", Files: []string{"go.mod"}}},
		Cache:  &extension.TaskCachePolicy{Enabled: &enabled, NoOutput: true},
	}
	testTask := task("test", nil, []string{cachekey.GenResourceID}, nil)
	testTask.Inputs["tests"] = extension.TaskInputPort{From: "project", Files: []string{"**/*_test.go"}}

	command := func(name string, steps ...extension.PipelineStep) *extension.JobDefinition {
		return &extension.JobDefinition{
			ExtensionName: "@test/pipeline", Name: name, Kind: "command", Command: "probe", Cache: true,
			PipelineSteps: steps,
		}
	}
	return &extension.ExtensionDescription{
		Name:    "@test/pipeline",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"build": command("build",
				extension.PipelineStep{ID: "config-merge", Task: "probe-config-merge", DependsOn: []string{"^config-merge"}},
				extension.PipelineStep{ID: "tidy", Task: "probe-tidy", DependsOn: []string{"^tidy", "config-merge"}},
				extension.PipelineStep{ID: "generate", Task: "probe-generate", DependsOn: []string{"^generate", "tidy"}},
				extension.PipelineStep{ID: "describe", Task: "probe-describe", DependsOn: []string{"^describe", "generate"}},
				extension.PipelineStep{ID: "compile", Task: "probe-compile", DependsOn: []string{"describe"}},
			),
			"test": command("test",
				extension.PipelineStep{ID: "config-merge", Task: "probe-config-merge-test", DependsOn: []string{"^config-merge"}},
				extension.PipelineStep{ID: "generate", Task: "probe-generate", DependsOn: []string{"^generate", "config-merge"}},
				extension.PipelineStep{ID: "describe", Task: "probe-describe", DependsOn: []string{"^describe", "generate"}},
				extension.PipelineStep{ID: "test", Task: "probe-test", DependsOn: []string{"describe"}},
			),
		},
		Tasks: map[string]extension.TaskDefinition{
			"probe-config-merge":      configMerge(".env.yaml"),
			"probe-config-merge-test": configMerge(".env.test.yaml"),
			"probe-tidy":              tidy,
			"probe-generate": task("generate", []string{cachekey.GenResourceID}, nil,
				probeDirectoryOutput(".gen", ".gen/conf"), "mode"),
			"probe-describe": task("describe", []string{"clients"}, []string{cachekey.GenResourceID},
				probeDirectoryOutput("clients"), "race"),
			"probe-compile": task("compile", []string{"dist"}, []string{cachekey.GenResourceID},
				probeDirectoryOutput("dist")),
			"probe-test": testTask,
		},
	}
}

// pipelineProbeProjects is an app that depends on a library, so the
// cross-project `^` references are part of every case.
func pipelineProbeProjects() []*workspace.Project {
	return []*workspace.Project{
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/pipeline"}},
		{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}, Extensions: []string{"@test/pipeline"}},
	}
}

func planPipelineProbe(
	t *testing.T,
	commands []string,
	ext *extension.ExtensionDescription,
	projects []*workspace.Project,
) []*jobmodel.ScheduledJob {
	t.Helper()
	ws := testWorkspace(t.TempDir(), projects...)
	planned, err := Plan(ws, commands, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan %v: %v", commands, err)
	}
	return planned
}

// probeDirectoryOutput declares one project-rooted directory output that cedes
// the excluded subpaths.
func probeDirectoryOutput(path string, excludes ...string) *extension.TaskDeclaration {
	return &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"out": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: path, Excludes: excludes},
	}}
}

// setStepBinding binds one input of one pipeline step without mutating the
// fixture's other commands.
func setStepBinding(ext *extension.ExtensionDescription, command, step, port string, binding extension.InputBinding) {
	def := *ext.Jobs[command]
	def.PipelineSteps = append([]extension.PipelineStep(nil), def.PipelineSteps...)
	for i := range def.PipelineSteps {
		if def.PipelineSteps[i].ID != step {
			continue
		}
		with := map[string]extension.InputBinding{port: binding}
		for name, existing := range def.PipelineSteps[i].With {
			with[name] = existing
		}
		def.PipelineSteps[i].With = with
	}
	ext.Jobs[command] = &def
}

func requirePlanKey(t *testing.T, byKey map[string]*jobmodel.ScheduledJob, key string) *jobmodel.ScheduledJob {
	t.Helper()
	job := byKey[key]
	if job == nil {
		t.Fatalf("plan does not contain %s; it holds %v", key, sortedPlanKeys(byKey))
	}
	return job
}

func sortedPlanKeys(byKey map[string]*jobmodel.ScheduledJob) []string {
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// planAncestors returns every node job waits for, through functional and
// ordering edges.
func planAncestors(byKey map[string]*jobmodel.ScheduledJob, job *jobmodel.ScheduledJob) map[string]bool {
	seen := make(map[string]bool)
	stack := append(append([]string(nil), job.DependsOn...), job.SerializeAfter...)
	for len(stack) > 0 {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[key] {
			continue
		}
		seen[key] = true
		if producer := byKey[key]; producer != nil {
			stack = append(stack, producer.DependsOn...)
			stack = append(stack, producer.SerializeAfter...)
		}
	}
	return seen
}

// planOrdered reports whether one of a and b waits for the other.
func planOrdered(byKey map[string]*jobmodel.ScheduledJob, a, b *jobmodel.ScheduledJob) bool {
	return planAncestors(byKey, a)[b.Key()] || planAncestors(byKey, b)[a.Key()]
}

// planShape is a plan's nodes, their functional and ordering edges and their
// shared execution ids, for comparing two plans.
func planShape(planned []*jobmodel.ScheduledJob) map[string]string {
	shape := make(map[string]string, len(planned))
	for _, job := range planned {
		shape[job.Key()] = fmt.Sprintf("dependsOn=%v serializeAfter=%v shared=%q",
			job.DependsOn, job.SerializeAfter, job.SharedExecution())
	}
	return shape
}

// requireShared fails unless the two nodes are one shared execution that
// leader leads: the follower runs after it.
func requireShared(t *testing.T, byKey map[string]*jobmodel.ScheduledJob, leaderKey, followerKey string) {
	t.Helper()
	leader := requirePlanKey(t, byKey, leaderKey)
	follower := requirePlanKey(t, byKey, followerKey)
	if leader.SharedExecution() == "" || leader.SharedExecution() != follower.SharedExecution() {
		t.Errorf("%s and %s are not one shared execution: %q, %q",
			leaderKey, followerKey, leader.SharedExecution(), follower.SharedExecution())
		return
	}
	if !slices.Contains(follower.SerializeAfter, leaderKey) {
		t.Errorf("%s does not run after its leader %s: serializeAfter %v", followerKey, leaderKey, follower.SerializeAfter)
	}
}

// TestSharedExecutions_BuildAndTestRunOneGenerateAndOneDescribe is the payload:
// a plan with both commands executes one generate and one describe per
// project. The follower takes its leader's place in the write order, so
// nothing on the test side waits for build's compile, and the readers of that
// place wait for the follower.
func TestSharedExecutions_BuildAndTestRunOneGenerateAndOneDescribe(t *testing.T) {
	t.Parallel()
	planned := planPipelineProbe(t, []string{"test", "build"}, pipelineProbeExtension(), pipelineProbeProjects())
	byKey := JobsByPlanKey(planned)

	for _, project := range []string{"/lib", "/app"} {
		for _, step := range []string{"generate", "describe"} {
			requireShared(t, byKey, project+":build~"+step, project+":test~"+step)
		}
		// The leader runs after the follower's config merge, through an
		// ordering edge only.
		generate := requirePlanKey(t, byKey, project+":build~generate")
		if !slices.Contains(generate.SerializeAfter, project+":test~config-merge") {
			t.Errorf("%s does not run after the test config merge: serializeAfter %v", generate.Key(), generate.SerializeAfter)
		}
		if slices.Contains(generate.DependsOn, project+":test~config-merge") {
			t.Errorf("%s depends on the test config merge: %v", generate.Key(), generate.DependsOn)
		}
		for key := range planAncestors(byKey, requirePlanKey(t, byKey, project+":test~test")) {
			if byKey[key] != nil && byKey[key].DisplayName() == "build~compile" {
				t.Errorf("%s:test~test waits for %s", project, key)
			}
		}
		compile := requirePlanKey(t, byKey, project+":build~compile")
		if !planAncestors(byKey, compile)[project+":test~generate"] {
			t.Errorf("%s reads the gen tree before the follower %s:test~generate captures it", compile.Key(), project)
		}
	}
}

// TestSharedExecutions_SingleCommandPlansAreUnchanged pins that a plan with one
// of the two commands has no shared execution and keeps its functional edges:
// there is no second command to share with.
func TestSharedExecutions_SingleCommandPlansAreUnchanged(t *testing.T) {
	t.Parallel()
	want := map[string]map[string][]string{
		"test": {
			"/lib:test~config-merge": {},
			"/lib:test~generate":     {"/lib:test~config-merge"},
			"/lib:test~describe":     {"/lib:test~generate"},
			"/lib:test~test":         {"/lib:test~describe"},
			"/app:test~config-merge": {"/lib:test~config-merge"},
			"/app:test~generate":     {"/app:test~config-merge", "/lib:test~generate"},
			"/app:test~describe":     {"/app:test~generate", "/lib:test~describe"},
			"/app:test~test":         {"/app:test~describe"},
		},
		"build": {
			"/lib:build~config-merge": {},
			"/lib:build~tidy":         {"/lib:build~config-merge"},
			"/lib:build~generate":     {"/lib:build~tidy"},
			"/lib:build~describe":     {"/lib:build~generate"},
			"/lib:build~compile":      {"/lib:build~describe"},
			"/app:build~config-merge": {"/lib:build~config-merge"},
			"/app:build~tidy":         {"/app:build~config-merge", "/lib:build~tidy"},
			"/app:build~generate":     {"/app:build~tidy", "/lib:build~generate"},
			"/app:build~describe":     {"/app:build~generate", "/lib:build~describe"},
			"/app:build~compile":      {"/app:build~describe"},
		},
	}
	for _, command := range []string{"test", "build"} {
		planned := planPipelineProbe(t, []string{command}, pipelineProbeExtension(), pipelineProbeProjects())
		got := make(map[string][]string, len(planned))
		for _, job := range planned {
			got[job.Key()] = append([]string{}, job.DependsOn...)
			if id := job.SharedExecution(); id != "" {
				t.Errorf("%s joined %q in a plan of %s alone", job.Key(), id, command)
			}
		}
		if !reflect.DeepEqual(got, want[command]) {
			t.Errorf("%s plan:\n got %v\nwant %v", command, got, want[command])
		}
	}
}

// TestSharedExecutions_PipelinePlanIsDeterministic pins that two Plan calls,
// and the two orders the commands can be typed in, produce one plan: the same
// edges and the same shared execution ids.
func TestSharedExecutions_PipelinePlanIsDeterministic(t *testing.T) {
	t.Parallel()
	first := planShape(planPipelineProbe(t, []string{"build", "test"}, pipelineProbeExtension(), pipelineProbeProjects()))
	second := planShape(planPipelineProbe(t, []string{"build", "test"}, pipelineProbeExtension(), pipelineProbeProjects()))
	reversed := planShape(planPipelineProbe(t, []string{"test", "build"}, pipelineProbeExtension(), pipelineProbeProjects()))
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two Plan calls disagree:\n%v\n%v", first, second)
	}
	if !reflect.DeepEqual(first, reversed) {
		t.Errorf("the command order changes the plan:\n%v\n%v", first, reversed)
	}
}

// TestSharedExecutions_KeysMatchEachCommandAlone pins that sharing moves no
// cache key and no functional edge: every node of a plan with both commands
// depends on what it depends on, and keys as it keys, in a plan of its own
// command alone. An entry one run stores is found by a run that plans another
// command set.
func TestSharedExecutions_KeysMatchEachCommandAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(projects []*workspace.Project)
	}{
		{name: "every pair shared"},
		{
			name: "a describe that stays separate",
			mutate: func(projects []*workspace.Project) {
				projects[1].Config = &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{"test": {"race": true}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ext := pipelineProbeExtension()
			projects := pipelineProbeProjects()
			if tc.mutate != nil {
				tc.mutate(projects)
			}
			root := t.TempDir()
			for _, project := range projects {
				writeProjectFile(t, root, project.Path, "go.mod", "module example.com/"+project.Name+"\n")
				writeProjectFile(t, root, project.Path, "main.go", "package "+project.Name+"\n")
				writeProjectFile(t, root, project.Path, "main_test.go", "package "+project.Name+"\n")
				writeProjectFile(t, root, project.Path, "putnami.json", `{"name":"`+project.Name+`"}`)
			}
			ws := testWorkspace(root, projects...)
			cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
			plan := func(commands ...string) ([]*jobmodel.ScheduledJob, map[string]string) {
				planned, err := Plan(ws, commands, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
				if err != nil {
					t.Fatalf("Plan %v: %v", commands, err)
				}
				keys, err := cachekey.PrecomputeKeys(ws, planned, nil, nil, cache, cachekey.CacheBypass{})
				if err != nil {
					t.Fatalf("PrecomputeKeys %v: %v", commands, err)
				}
				return planned, keys
			}
			combined, combinedKeys := plan("build", "test")
			shared := 0
			for _, job := range combined {
				if job.SharedExecution() != "" {
					shared++
				}
			}
			if shared == 0 {
				t.Fatal("control: the plan has no shared execution, so the comparison proves nothing")
			}
			aloneKeys := make(map[string]string)
			aloneDeps := make(map[string][]string)
			for _, command := range []string{"test", "build"} {
				planned, keys := plan(command)
				for _, job := range planned {
					aloneDeps[job.Key()] = job.DependsOn
				}
				for key, hash := range keys {
					aloneKeys[key] = hash
				}
			}
			keyed := 0
			for _, job := range combined {
				if got, want := combinedKeys[job.Key()], aloneKeys[job.Key()]; got != want {
					t.Errorf("%s keys %q beside the other command, %q alone", job.Key(), got, want)
				} else if got != "" {
					keyed++
				}
				if !slices.Equal(job.DependsOn, aloneDeps[job.Key()]) {
					t.Errorf("%s depends on %v beside the other command, %v alone", job.Key(), job.DependsOn, aloneDeps[job.Key()])
				}
			}
			if keyed == 0 {
				t.Fatal("control: the plan keyed no node")
			}
		})
	}
}

// TestSharedExecutions_KeepDifferentWorkApart covers each way two nodes of one
// step can be different work: the members of a group must be the same task over
// the same declared inputs after the same work.
func TestSharedExecutions_KeepDifferentWorkApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(ext *extension.ExtensionDescription, projects []*workspace.Project)
		// separate are the test nodes that must not share.
		separate []string
		// shared are the test nodes that must still share with build's.
		shared []string
	}{
		{
			name: "a literal binding on the test generate",
			mutate: func(ext *extension.ExtensionDescription, _ []*workspace.Project) {
				setStepBinding(ext, "test", "generate", "mode", extension.InputBinding{Value: "test", HasValue: true})
			},
			separate: []string{"/lib:test~generate", "/lib:test~describe", "/app:test~generate", "/app:test~describe"},
		},
		{
			name: "a race setting only the test command has",
			mutate: func(_ *extension.ExtensionDescription, projects []*workspace.Project) {
				projects[1].Config = &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{"test": {"race": true}}}
			},
			separate: []string{"/app:test~describe"},
			shared:   []string{"/lib:test~generate", "/lib:test~describe", "/app:test~generate"},
		},
		{
			name: "an upstream generate that stays separate",
			mutate: func(_ *extension.ExtensionDescription, projects []*workspace.Project) {
				projects[0].Config = &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{"test": {"mode": "test"}}}
			},
			separate: []string{"/lib:test~generate", "/lib:test~describe", "/app:test~generate", "/app:test~describe"},
		},
		{
			name: "a producer that declares a write",
			mutate: func(ext *extension.ExtensionDescription, _ []*workspace.Project) {
				merge := ext.Tasks["probe-config-merge-test"]
				merge.Writes = []extension.ResourceRef{{ID: "conf"}}
				ext.Tasks["probe-config-merge-test"] = merge
			},
			separate: []string{"/lib:test~generate", "/lib:test~describe", "/app:test~generate", "/app:test~describe"},
		},
		{
			name: "a nondeterministic task",
			mutate: func(ext *extension.ExtensionDescription, _ []*workspace.Project) {
				describe := ext.Tasks["probe-describe"]
				describe.Cache = &extension.TaskCachePolicy{Enabled: describe.Cache.Enabled}
				ext.Tasks["probe-describe"] = describe
			},
			separate: []string{"/lib:test~describe", "/app:test~describe"},
			shared:   []string{"/lib:test~generate", "/app:test~generate"},
		},
		{
			name: "a task without a declared write",
			mutate: func(ext *extension.ExtensionDescription, _ []*workspace.Project) {
				describe := ext.Tasks["probe-describe"]
				describe.Writes = nil
				ext.Tasks["probe-describe"] = describe
			},
			separate: []string{"/lib:test~describe", "/app:test~describe"},
			shared:   []string{"/lib:test~generate", "/app:test~generate"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ext := pipelineProbeExtension()
			projects := pipelineProbeProjects()
			tc.mutate(ext, projects)
			byKey := JobsByPlanKey(planPipelineProbe(t, []string{"build", "test"}, ext, projects))
			for _, key := range tc.separate {
				if id := requirePlanKey(t, byKey, key).SharedExecution(); id != "" {
					t.Errorf("%s joined %q; want it executed on its own", key, id)
				}
			}
			for _, key := range tc.shared {
				project, step, _ := cutPlanKey(key)
				requireShared(t, byKey, project+":build~"+step, key)
			}
		})
	}
}

// cutPlanKey splits "<project>:<command>~<step>" into project and step.
func cutPlanKey(key string) (project, step string, ok bool) {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '~' {
			step = key[i+1:]
			for j := i - 1; j >= 0; j-- {
				if key[j] == ':' {
					return key[:j], step, true
				}
			}
		}
	}
	return "", "", false
}

// TestSharedExecutions_KeepTheOrderOfAnotherCommand pins that a group stays
// apart when a node of another command reads what it writes and declares a
// write of its own. The probe's `clientgen` command follows build, as the
// clientgen command does, and its generator reads the project's gen and writes
// its own resource. Unshared, test's generate is the later gen writer that
// waits for the generator, and the test steps follow it.
func TestSharedExecutions_KeepTheOrderOfAnotherCommand(t *testing.T) {
	t.Parallel()
	ext := pipelineProbeExtension()
	ext.Jobs["clientgen"] = &extension.JobDefinition{
		ExtensionName: "@test/pipeline", Name: "clientgen", Kind: "command", Command: "probe", Cache: true,
		CommandDependsOn: []string{"build"},
		PipelineSteps:    []extension.PipelineStep{{ID: "generate-go", Task: "probe-clientgen"}},
	}
	generator := ext.Tasks["probe-describe"]
	generator.Args = []string{"clientgen"}
	generator.Writes = []extension.ResourceRef{{ID: "clients-go"}}
	generator.Declares = probeDirectoryOutput("client-go")
	ext.Tasks["probe-clientgen"] = generator
	projects := pipelineProbeProjects()[:1]

	requireShared(t, JobsByPlanKey(planPipelineProbe(t, []string{"build", "test"}, ext, projects)),
		"/lib:build~generate", "/lib:test~generate")

	byKey := JobsByPlanKey(planPipelineProbe(t, []string{"build", "test", "clientgen"}, ext, projects))
	if id := requirePlanKey(t, byKey, "/lib:test~generate").SharedExecution(); id != "" {
		t.Errorf("/lib:test~generate joined %q beside a writing node of another command", id)
	}
	test := requirePlanKey(t, byKey, "/lib:test~test")
	clientgen := requirePlanKey(t, byKey, "/lib:clientgen~generate-go")
	if !planOrdered(byKey, test, clientgen) {
		t.Errorf("%s and %s are unordered", clientgen.Key(), test.Key())
	}
}

// pipelineProbeWithInfra adds to the probe's build an infra step after the
// compile that writes the project's gen and clients, and keeps test's describe
// apart from build's with a race setting only the test command has. Sharing
// the generate pair puts test's describe in the leader's gen block, so infra,
// the later gen writer, waits for it, while infra precedes it among the
// clients writers: the shared plan has a cycle the plan without sharing does
// not.
func pipelineProbeWithInfra() (*extension.ExtensionDescription, []*workspace.Project) {
	ext := pipelineProbeExtension()
	def := *ext.Jobs["build"]
	def.PipelineSteps = append(append([]extension.PipelineStep(nil), def.PipelineSteps...),
		extension.PipelineStep{ID: "infra", Task: "probe-infra", DependsOn: []string{"compile"}})
	ext.Jobs["build"] = &def
	infra := ext.Tasks["probe-describe"]
	infra.Args = []string{"infra"}
	infra.Writes = []extension.ResourceRef{{ID: cachekey.GenResourceID}, {ID: "clients"}}
	infra.Reads = nil
	infra.Declares = probeDirectoryOutput("infra")
	ext.Tasks["probe-infra"] = infra
	projects := pipelineProbeProjects()[:1]
	projects[0].Config = &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{"test": {"race": true}}}
	return ext, projects
}

// TestSharedExecutions_DroppedWhenTheirPlanCannotBeScheduled pins that sharing
// never turns a plan that schedules into one that does not: the plan of
// pipelineProbeWithInfra keeps no shared execution, schedules, and records one
// warning that says so. A plan that keeps its sharing records none.
func TestSharedExecutions_DroppedWhenTheirPlanCannotBeScheduled(t *testing.T) {
	t.Parallel()
	ext, projects := pipelineProbeWithInfra()
	plan := func() ([]*jobmodel.ScheduledJob, *workspace.Workspace) {
		ws := testWorkspace(t.TempDir(), projects...)
		planned, err := Plan(ws, []string{"build", "test"}, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		return planned, ws
	}
	planned, ws := plan()
	if len(ws.Warnings) != 1 || ws.Warnings[0].Code != workspace.WarningCodeSharedExecutionsDropped {
		t.Errorf("warnings = %+v, want the one shared-executions-dropped warning", ws.Warnings)
	}
	byKey := JobsByPlanKey(planned)
	requirePlanKey(t, byKey, "/lib:build~infra")
	for _, job := range planned {
		if id := job.SharedExecution(); id != "" {
			t.Errorf("%s keeps %q in a plan whose shared ordering has a cycle", job.Key(), id)
		}
	}
	if err := ValidatePlanDAG(planned); err != nil {
		t.Errorf("the plan without sharing does not schedule: %v", err)
	}

	// Without infra's clients write the shared plan schedules, and sharing
	// stays.
	infra := ext.Tasks["probe-infra"]
	infra.Writes = []extension.ResourceRef{{ID: cachekey.GenResourceID}}
	ext.Tasks["probe-infra"] = infra
	planned, ws = plan()
	requireShared(t, JobsByPlanKey(planned), "/lib:build~generate", "/lib:test~generate")
	if len(ws.Warnings) != 0 {
		t.Errorf("a plan that keeps its sharing warns %+v", ws.Warnings)
	}
}

// planRealGoProjects plans the CLI and an app sample against the real Go
// extension manifest.
func planRealGoProjects(t *testing.T, commands []string, params ...extension.ParamMap) []*jobmodel.ScheduledJob {
	t.Helper()
	repoRoot := findJobsRepoRoot(t)
	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("go/extension/putnami.extension.json did not load")
	}
	goExtension.RelPath = "go/extension"
	for _, job := range goExtension.Jobs {
		job.ExtensionPath = goExtension.RelPath
	}
	var projects []*workspace.Project
	for _, rel := range []string{"tooling/cli", "go/samples/simple-api"} {
		config := wsproto.LoadProjectConfig(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if config == nil {
			t.Fatalf("%s/putnami.json did not load", rel)
		}
		projects = append(projects, &workspace.Project{
			ID: "/" + rel, Name: config.Name, Type: config.Type, Path: rel,
			Tags:       append([]string(nil), config.Tags...),
			Extensions: append([]string(nil), config.Extensions...),
			Config:     config,
		})
	}
	ws := workspace.NewWorkspace(repoRoot, &wsproto.Config{}, projects)
	var commandParams extension.ParamMap
	if len(params) > 0 {
		commandParams = params[0]
	}
	planned, err := Plan(ws, commands, projects, []*extension.ExtensionDescription{goExtension}, commandParams, nil, nil)
	if err != nil {
		t.Fatalf("Plan %v: %v", commands, err)
	}
	return planned
}

// TestSharedExecutions_RealGoManifest plans the real Go manifest. The CLI's
// test options widen every test key with the documentation and every
// manifest, and build declares readonly and trimpath defaults test does not,
// so this is the test that shows the generate and describe of one project are
// the same work under both commands, that the manifest leaves the test
// pipeline nothing that tells them apart, and that the shared plan schedules
// without falling back.
func TestSharedExecutions_RealGoManifest(t *testing.T) {
	t.Parallel()
	for _, params := range []extension.ParamMap{nil, {"skip-deps": true}} {
		byKey := JobsByPlanKey(planRealGoProjects(t, []string{"lint", "test", "build"}, params))
		for _, project := range []string{"/tooling/cli", "/go/samples/simple-api"} {
			requireShared(t, byKey, project+":build~generate", project+":test~generate")
			testJob := requirePlanKey(t, byKey, project+":test~test")
			for key := range planAncestors(byKey, testJob) {
				if byKey[key] != nil && byKey[key].DisplayName() == "build~compile" {
					t.Errorf("%v: %s test~test waits for %s", params, project, key)
				}
			}
		}
		requireShared(t, byKey, "/go/samples/simple-api:build~describe", "/go/samples/simple-api:test~describe")
	}

	// The same step planned by each command alone is the same work.
	testAlone := JobsByPlanKey(planRealGoProjects(t, []string{"test"}))
	buildAlone := JobsByPlanKey(planRealGoProjects(t, []string{"build"}))
	ws := workspace.NewWorkspace(findJobsRepoRoot(t), &wsproto.Config{}, nil)
	for _, key := range []string{
		"/tooling/cli:%s~generate",
		"/go/samples/simple-api:%s~generate",
		"/go/samples/simple-api:%s~describe",
	} {
		testJob := requirePlanKey(t, testAlone, fmt.Sprintf(key, "test"))
		buildJob := requirePlanKey(t, buildAlone, fmt.Sprintf(key, "build"))
		if len(testJob.Step.With) != 0 {
			t.Errorf("%s carries a with binding %v; the test pipeline must not tell it apart from build's", testJob.Key(), testJob.Step.With)
		}
		testParts := executionIdentityParts(ws, testJob, nil)
		buildParts := executionIdentityParts(ws, buildJob, nil)
		if !reflect.DeepEqual(testParts, buildParts) {
			t.Errorf("%s and %s are different work:\n%v\n%v", testJob.Key(), buildJob.Key(), testParts, buildParts)
		}
	}
}

// TestSharedExecutions_RealGoManifestWithPackage plans the real Go manifest
// over an application under `test,build,package --deployment`. Package's
// generate is the same work as build's and test's, so the three are one shared
// execution, and package's steps follow the leader's write like test's do. The
// deployment step declares its gen write, so it is ordered against every step
// of the run that reads or writes the project's gen.
func TestSharedExecutions_RealGoManifestWithPackage(t *testing.T) {
	t.Parallel()
	planned := planRealGoApplication(t, []string{"test", "build", "package"}, extension.ParamMap{"deployment": true})
	byKey := JobsByPlanKey(planned)
	requireShared(t, byKey, "/subject:build~generate", "/subject:test~generate")
	requireShared(t, byKey, "/subject:build~generate", "/subject:package~generate")

	deployment := requirePlanKey(t, byKey, "/subject:package~deployment")
	accessors := 0
	for _, job := range planned {
		refs := append(append([]extension.ResourceRef(nil), job.JobDef.Writes...), job.JobDef.Reads...)
		if job == deployment || !slices.ContainsFunc(refs, func(ref extension.ResourceRef) bool { return ref.ID == cachekey.GenResourceID }) {
			continue
		}
		accessors++
		if !planOrdered(byKey, deployment, job) {
			t.Errorf("%s and %s are unordered: the deployment step writes .gen/deployment.json", deployment.Key(), job.Key())
		}
	}
	if accessors == 0 {
		t.Fatal("control: no other step of the plan reads or writes the project's gen")
	}
}

// planRealGoApplication plans one Go application against the real Go
// extension manifest.
func planRealGoApplication(t *testing.T, commands []string, params extension.ParamMap) []*jobmodel.ScheduledJob {
	t.Helper()
	goExtension := loadGoPlannerExtension(t)
	root := t.TempDir()
	writeProjectFile(t, root, "subject", "go.mod",
		"module example.com/subject\n\ngo 1.25\n\nrequire go.putnami.dev/app v0.0.0\n")
	writeProjectFile(t, root, "subject", "subject.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, root, "subject", wsproto.ConfigFilename, `{}`)
	project := &workspace.Project{
		ID: "/subject", Name: "subject", Path: "subject", Type: "application",
		Extensions: []string{"@putnami/go"},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "subject")),
	}
	planned, err := Plan(testWorkspace(root, project), commands, []*workspace.Project{project},
		[]*extension.ExtensionDescription{goExtension}, params, nil, nil)
	if err != nil {
		t.Fatalf("Plan %v: %v", commands, err)
	}
	return planned
}

// TestSharedExecutions_RealGoManifestLeavesNoConflictUnordered plans the real
// Go manifest under `build,test,package`, where the generate of each command is
// one shared execution of three members. A member that misses its own entry
// executes or restores while another one captures, so every two members of a
// group are ordered, and so are every two steps of the plan that access one
// resource when either of them writes it.
func TestSharedExecutions_RealGoManifestLeavesNoConflictUnordered(t *testing.T) {
	t.Parallel()
	for _, params := range []extension.ParamMap{{"archives": true}, {"deployment": true}} {
		planned := planRealGoApplication(t, []string{"build", "test", "package"}, params)
		byKey := JobsByPlanKey(planned)
		requireShared(t, byKey, "/subject:build~generate", "/subject:package~generate")
		requireShared(t, byKey, "/subject:build~generate", "/subject:test~generate")

		groups := make(map[string][]*jobmodel.ScheduledJob)
		for _, job := range planned {
			if id := job.SharedExecution(); id != "" {
				groups[id] = append(groups[id], job)
			}
		}
		largest := 0
		for _, members := range groups {
			largest = max(largest, len(members))
			for i, a := range members {
				for _, b := range members[i+1:] {
					if !planOrdered(byKey, a, b) {
						t.Errorf("%v: the members %s and %s of %s are unordered", params, a.Key(), b.Key(), a.SharedExecution())
					}
				}
			}
		}
		if largest < 3 {
			t.Fatalf("%v: control: no shared execution has three members", params)
		}

		type access struct {
			job   *jobmodel.ScheduledJob
			write bool
		}
		accesses := make(map[string][]access)
		for _, job := range planned {
			for _, ref := range job.JobDef.Reads {
				key := ResourceConflictKey(job.Project.ID, ref)
				accesses[key] = append(accesses[key], access{job: job})
			}
			for _, ref := range job.JobDef.Writes {
				key := ResourceConflictKey(job.Project.ID, ref)
				accesses[key] = append(accesses[key], access{job: job, write: true})
			}
		}
		genWriters := 0
		for key, list := range accesses {
			for i, a := range list {
				if a.write && key == ResourceConflictKey("/subject", extension.ResourceRef{ID: cachekey.GenResourceID}) {
					genWriters++
				}
				for _, b := range list[i+1:] {
					if a.job == b.job || (!a.write && !b.write) {
						continue
					}
					if !planOrdered(byKey, a.job, b.job) {
						t.Errorf("%v: %s and %s both access %s, one writes it, and they are unordered", params, a.job.Key(), b.job.Key(), key)
					}
				}
			}
		}
		if genWriters < 3 {
			t.Fatalf("%v: control: %d steps write the project's gen, want the three generates at least", params, genWriters)
		}
	}
}
