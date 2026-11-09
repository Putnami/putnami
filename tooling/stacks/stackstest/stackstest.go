// Package stackstest is the conformance kit of the stack core: every stack
// extension runs CheckManifest on its manifest, so a manifest that drifts
// from its profile or from the core's task shape fails the extension's own
// gate.
package stackstest

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/stacks"
)

// CheckManifest checks the manifest at path against the stack: the protocol's
// own validation and contract, then the shape every stack extension shares:
// the cache of each task is on, or off for a delegated profile.
func CheckManifest(t testing.TB, path string, s stacks.Stack) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the extension's own committed manifest
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m, diags := proto.ParseManifest(data)
	if len(diags) > 0 {
		t.Fatalf("%s does not parse: %v", path, diags)
	}
	if diags := proto.FullValidateManifest(m); len(diags) > 0 {
		t.Fatalf("%s does not validate: %v", path, diags)
	}
	if want := proto.RequiredCLIContract(m); m.CLIContract != want {
		t.Errorf("cliContract = %d, want %d", m.CLIContract, want)
	}

	var doc manifest
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if doc.Name != s.Extension {
		t.Errorf("name = %q, want %q", doc.Name, s.Extension)
	}
	if got, want := sorted(doc.Workspace.Markers), sorted(s.Profile.Markers); !reflect.DeepEqual(got, want) {
		t.Errorf("workspace.markers = %v, want the profile's %v", got, want)
	}
	// The probe reads the markers and the toolchain files; a file it reads
	// that the adapter does not hash would leave its answer stale.
	for _, f := range s.Profile.Markers {
		if !slices.Contains(doc.Workspace.Inputs, f) {
			t.Errorf("workspace.inputs %v does not hold the marker %s", doc.Workspace.Inputs, f)
		}
	}
	for _, v := range s.Profile.Applied() {
		for _, f := range v.Toolchain.AllFiles() {
			if !slices.Contains(doc.Workspace.Inputs, f.Path) {
				t.Errorf("workspace.inputs %v does not hold the toolchain file %s", doc.Workspace.Inputs, f.Path)
			}
		}
	}
	// A runtime command shares the runtime's argument space with the jobs
	// and the control calls.
	jobs := slices.Clone(stacks.TaskNames)
	for _, task := range doc.Tasks {
		if len(task.Args) > 0 {
			jobs = append(jobs, task.Args[0])
		}
	}
	for name := range s.Commands {
		if slices.Contains(jobs, name) || strings.HasPrefix(name, "__") || strings.HasPrefix(name, "-") {
			t.Errorf("runtime command %q shadows a job of the manifest or a control call", name)
		}
	}
	if got, want := sorted(doc.Runtime.InputSelectors), s.Selectors(); !reflect.DeepEqual(got, want) {
		t.Errorf("runtime.inputSelectors = %v, want %v", got, want)
	}
	for name := range doc.Commands {
		if !s.Profile.HasTask(name) && slices.Contains(stacks.TaskNames, name) {
			t.Errorf("command %s runs a task the profile does not declare", name)
		}
	}
	for _, task := range stacks.TaskNames {
		if !s.Profile.HasTask(task) {
			continue
		}
		checkTask(t, doc, s, task)
	}
}

func checkTask(t testing.TB, doc manifest, s stacks.Stack, task string) {
	t.Helper()
	command, ok := doc.Commands[task]
	i := slices.IndexFunc(command.Run, func(step commandStep) bool { return step.ID == task })
	if !ok || i < 0 {
		t.Errorf("command %s: want a step %s, got %+v", task, task, command.Run)
		return
	}
	step := command.Run[i]
	if s.Profile.Delegated {
		if len(command.Run) != 2 || i != 1 {
			t.Errorf("command %s: want the %s step then the %s step, got %+v", task, stacks.TaskGraph, task, command.Run)
			return
		}
		checkGraphStep(t, doc, task, command.Run[0])
		// The tool runs on the selected projects alone: the graph step
		// carries the edge, so a dependency runs its graph step, never its
		// tool.
		if !slices.Equal(step.DependsOn, []string{stacks.TaskGraph}) {
			t.Errorf("command %s: step %s depends on %v, want its own %s step alone", task, step.ID, step.DependsOn, stacks.TaskGraph)
		}
	} else if !slices.Contains(step.DependsOn, "^"+step.ID) {
		t.Errorf("command %s: step %s depends on no ^%s: a change upstream would not rerun it", task, step.ID, step.ID)
	}
	decl, ok := doc.Tasks[step.Task]
	if !ok {
		t.Errorf("command %s names no declared task %s", task, step.Task)
		return
	}
	if want := []string{task, "--report-dir", "{outputRoot}"}; decl.Command != "{extensionRuntime}" || !reflect.DeepEqual(decl.Args, want) || decl.Cwd != "{projectRoot}" {
		t.Errorf("task %s runs %s %v in %s, want {extensionRuntime} %v in {projectRoot}", step.Task, decl.Command, decl.Args, decl.Cwd, want)
	}
	root := s.Profile.ShippedRootPatterns(task)
	var selector, workspace bool
	for name, in := range decl.Inputs {
		switch in.From {
		case "project":
			if slices.Contains(in.Files, s.Selector(task)) {
				selector = true
			}
		case "workspace":
			workspace = true
			if got, want := sorted(in.Files), sorted(root); !reflect.DeepEqual(got, want) {
				t.Errorf("task %s input %s = %v, want the stack-root files of every variant %v", step.Task, name, got, want)
			}
		}
	}
	if host, ok := decl.Inputs["hostPlatform"]; s.Profile.HostDependent(task) && (!ok || host.From != "runtime") {
		t.Errorf("task %s builds host-dependent targets but reads no hostPlatform runtime input", step.Task)
	}
	if !selector {
		t.Errorf("task %s reads no project input that names %s", step.Task, s.Selector(task))
	}
	if !workspace && len(root) > 0 {
		t.Errorf("task %s reads no workspace input for the stack-root files %v", step.Task, root)
	}
	// Every task of a delegated stack is uncached; every task of any other
	// stack is cached.
	if enabled := cacheEnabled(decl.Cache); enabled == s.Profile.Delegated {
		t.Errorf("task %s: cache.enabled = %v, want %v for a profile whose delegated is %v", step.Task, enabled, !s.Profile.Delegated, s.Profile.Delegated)
	}
	out, ok := decl.Declares.Outputs["reports"]
	if !ok || out.Kind != "directory" || out.Root != "command-output" || out.Path != task {
		t.Errorf("task %s declares no command-output directory %q for its reports: %+v", step.Task, task, out)
	}
}

// cacheEnabled reads a task's cache member the way the protocol does: a
// boolean, or a policy object; an absent member or enabled is on.
func cacheEnabled(raw json.RawMessage) bool {
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		return on
	}
	var policy proto.TaskCachePolicy
	if len(raw) > 0 && json.Unmarshal(raw, &policy) != nil {
		return false
	}
	return policy.IsEnabled()
}

// checkGraphStep checks the graph step of a delegated command: it depends on
// the graph step upstream, and its task runs the runtime's graph job
// uncached.
func checkGraphStep(t testing.TB, doc manifest, command string, step commandStep) {
	t.Helper()
	if step.ID != stacks.TaskGraph || !slices.Equal(step.DependsOn, []string{"^" + stacks.TaskGraph}) {
		t.Errorf("command %s: the first step is %s depending on %v, want %s depending on ^%s", command, step.ID, step.DependsOn, stacks.TaskGraph, stacks.TaskGraph)
	}
	decl, ok := doc.Tasks[step.Task]
	if !ok {
		t.Errorf("command %s names no declared task %s", command, step.Task)
		return
	}
	if want := []string{stacks.TaskGraph}; decl.Command != "{extensionRuntime}" || !reflect.DeepEqual(decl.Args, want) || cacheEnabled(decl.Cache) {
		t.Errorf("task %s runs %s %v cached=%v, want {extensionRuntime} %v uncached", step.Task, decl.Command, decl.Args, cacheEnabled(decl.Cache), want)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// manifest is the part of a manifest the kit reads.
type manifest struct {
	Name    string `json:"name"`
	Runtime struct {
		InputSelectors []string `json:"inputSelectors"`
	} `json:"runtime"`
	Workspace struct {
		Markers []string `json:"markers"`
		Inputs  []string `json:"inputs"`
	} `json:"workspace"`
	Commands map[string]struct {
		Run []commandStep `json:"run"`
	} `json:"commands"`
	Tasks map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Cwd     string   `json:"cwd"`
		Inputs  map[string]struct {
			From  string   `json:"from"`
			Files []string `json:"files"`
		} `json:"inputs"`
		Cache    json.RawMessage `json:"cache"`
		Declares struct {
			Outputs map[string]struct {
				Kind string `json:"kind"`
				Root string `json:"root"`
				Path string `json:"path"`
			} `json:"outputs"`
		} `json:"declares"`
	} `json:"tasks"`
}

// commandStep is one step of a manifest command.
type commandStep struct {
	ID        string   `json:"id"`
	Task      string   `json:"task"`
	DependsOn []string `json:"dependsOn"`
}
