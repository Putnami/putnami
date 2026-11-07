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
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/stacks"
)

// CheckManifest checks the manifest at path against the stack: the protocol's
// own validation and contract, then the shape every stack extension shares.
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
	if got, want := sorted(doc.Runtime.InputSelectors), s.Selectors(); !reflect.DeepEqual(got, want) {
		t.Errorf("runtime.inputSelectors = %v, want %v", got, want)
	}
	for name := range doc.Commands {
		if _, ok := s.Profile.Tasks[name]; !ok && slices.Contains(stacks.TaskNames, name) {
			t.Errorf("command %s runs a task the profile does not declare", name)
		}
	}
	for _, task := range stacks.TaskNames {
		if _, ok := s.Profile.Tasks[task]; !ok {
			continue
		}
		checkTask(t, doc, s, task)
	}
}

func checkTask(t testing.TB, doc manifest, s stacks.Stack, task string) {
	t.Helper()
	command, ok := doc.Commands[task]
	if !ok || len(command.Run) != 1 {
		t.Errorf("command %s: want one step, got %+v", task, command.Run)
		return
	}
	step := command.Run[0]
	if !slices.Contains(step.DependsOn, "^"+step.ID) {
		t.Errorf("command %s: step %s depends on no ^%s: a change upstream would not rerun it", task, step.ID, step.ID)
	}
	decl, ok := doc.Tasks[step.Task]
	if !ok {
		t.Errorf("command %s names no declared task %s", task, step.Task)
		return
	}
	if want := []string{task, "--output", "{outputRoot}"}; decl.Command != "{extensionRuntime}" || !reflect.DeepEqual(decl.Args, want) || decl.Cwd != "{projectRoot}" {
		t.Errorf("task %s runs %s %v in %s, want {extensionRuntime} %v in {projectRoot}", step.Task, decl.Command, decl.Args, decl.Cwd, want)
	}
	_, root := s.Profile.TaskPatterns(task)
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
				t.Errorf("task %s input %s = %v, want the profile's stack-root files %v", step.Task, name, got, want)
			}
		}
	}
	if !selector {
		t.Errorf("task %s reads no project input that names %s", step.Task, s.Selector(task))
	}
	if !workspace && len(root) > 0 {
		t.Errorf("task %s reads no workspace input for the stack-root files %v", step.Task, root)
	}
	out, ok := decl.Declares.Outputs["reports"]
	if !ok || out.Kind != "directory" || out.Root != "command-output" || out.Path != task {
		t.Errorf("task %s declares no command-output directory %q for its reports: %+v", step.Task, task, out)
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
	} `json:"workspace"`
	Commands map[string]struct {
		Run []struct {
			ID        string   `json:"id"`
			Task      string   `json:"task"`
			DependsOn []string `json:"dependsOn"`
		} `json:"run"`
	} `json:"commands"`
	Tasks map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Cwd     string   `json:"cwd"`
		Inputs  map[string]struct {
			From  string   `json:"from"`
			Files []string `json:"files"`
		} `json:"inputs"`
		Declares struct {
			Outputs map[string]struct {
				Kind string `json:"kind"`
				Root string `json:"root"`
				Path string `json:"path"`
			} `json:"outputs"`
		} `json:"declares"`
	} `json:"tasks"`
}
