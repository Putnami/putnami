package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

type tscManifestStep struct {
	ID         string         `json:"id"`
	Task       string         `json:"task"`
	DependsOn  []string       `json:"dependsOn"`
	If         string         `json:"if"`
	Activation map[string]any `json:"activation"`
}

type tscManifest struct {
	Commands map[string]struct {
		Flags map[string]struct {
			Default any      `json:"default"`
			Choices []string `json:"choices"`
		} `json:"flags"`
		Run []tscManifestStep `json:"run"`
	} `json:"commands"`
	Tasks map[string]struct {
		Inputs map[string]struct {
			From  string   `json:"from"`
			Files []string `json:"files"`
		} `json:"inputs"`
		Outputs  map[string]any `json:"outputs"`
		Writes   []any          `json:"writes"`
		Reads    []any          `json:"reads"`
		Declares struct {
			Outputs map[string]struct {
				Kind             string `json:"kind"`
				Root             string `json:"root"`
				PathFrom         string `json:"pathFrom"`
				OptionalEmpty    bool   `json:"optionalEmpty"`
				ReadByDependents bool   `json:"readByDependents"`
			} `json:"outputs"`
		} `json:"declares"`
	} `json:"tasks"`
}

func readTscManifest(t *testing.T) tscManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m tscManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func steps(m tscManifest, command string) map[string]tscManifestStep {
	out := map[string]tscManifestStep{}
	for _, step := range m.Commands[command].Run {
		out[step.ID] = step
	}
	return out
}

// TestTscStepsArePlannedOnlyForATscProject pins the plan of the compiler
// choice: build, test, serve, run and package plan build-tsc as tsc when the
// compiler is tsc, and as tsc-auto, spliced out of a project whose
// package.json never names tsc, under auto. The Bun transpile and types steps
// of build are not planned for an explicit tsc.
func TestTscStepsArePlannedOnlyForATscProject(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-compiler-choice", "a-tsc-project-plans-no-bun-transpile-or-types")
	m := readTscManifest(t)
	for _, command := range []string{"build", "test", "serve", "run", "package"} {
		flag, ok := m.Commands[command].Flags["compiler"]
		if !ok || flag.Default != "auto" || !slices.Equal(flag.Choices, []string{"auto", "bun", "tsc"}) {
			t.Errorf("%s compiler flag = %+v, want auto, bun or tsc, auto by default", command, flag)
		}
	}
	// package builds nothing when it assembles an image built before.
	guards := map[string]string{"build": "", "test": "", "serve": "", "run": "", "package": " && !params.image"}
	for command, guard := range guards {
		run := steps(m, command)
		auto, explicit := run["tsc-auto"], run["tsc"]
		if auto.Task != "build-tsc" || auto.If != "params.compiler == 'auto'"+guard || auto.Activation == nil {
			t.Errorf("%s~tsc-auto = %+v, want build-tsc under auto with an activation gate", command, auto)
		}
		if explicit.Task != "build-tsc" || explicit.If != "params.compiler == 'tsc'"+guard || explicit.Activation != nil {
			t.Errorf("%s~tsc = %+v, want build-tsc under tsc with no gate", command, explicit)
		}
		// tsc reads what generate writes into the project.
		for _, step := range []tscManifestStep{auto, explicit} {
			if !slices.Contains(step.DependsOn, "generate") {
				t.Errorf("%s~%s depends on %v, want its project's generate", command, step.ID, step.DependsOn)
			}
		}
	}
	build := steps(m, "build")
	for _, id := range []string{"transpile", "types"} {
		if !strings.HasSuffix(build[id].If, ") && params.compiler != 'tsc'") {
			t.Errorf("build~%s if = %q, want it off for an explicit tsc", id, build[id].If)
		}
	}
	if !slices.Contains(build["infra"].DependsOn, "tsc") || !slices.Contains(build["infra"].DependsOn, "tsc-auto") {
		t.Errorf("build~infra depends on %v, want both tsc steps", build["infra"].DependsOn)
	}
}

// TestTscOutputsRestoreWhereDependentsReadThem pins the outputs of
// build-tsc: pathFrom outputs under the project, on the ports the task
// reports, which the engine captures and restores at the reported path, and
// the outDir and declarationDir its dependents read.
func TestTscOutputsRestoreWhereDependentsReadThem(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-output-restores-at-outdir", "a-cache-hit-restores-a-deleted-outdir")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-output-restores-at-outdir", "a-dependent-builds-after-its-tsc-library")
	task := readTscManifest(t).Tasks["build-tsc"]
	// build~tsc and test~tsc of one project write the same outDir: the write
	// resource runs them one after the other.
	if !slices.Equal(task.Writes, []any{"tsc-output"}) || !slices.Equal(task.Reads, []any{"gen", "sources"}) {
		t.Errorf("build-tsc writes %v and reads %v, want tsc-output, and gen and sources", task.Writes, task.Reads)
	}
	want := map[string]struct {
		kind string
		read bool
	}{
		TscOutDirPort:         {"directory", true},
		TscDeclarationDirPort: {"directory", true},
		TscBuildInfoPort:      {"file", false},
	}
	if len(task.Declares.Outputs) != len(want) {
		t.Fatalf("build-tsc declares %d outputs, want %d", len(task.Declares.Outputs), len(want))
	}
	for id, w := range want {
		output := task.Declares.Outputs[id]
		if output.PathFrom != id || output.Root != "" || output.Kind != w.kind || !output.OptionalEmpty || output.ReadByDependents != w.read {
			t.Errorf("build-tsc output %s = %+v, want a %s on port %s under the project, optional, readByDependents=%t", id, output, w.kind, id, w.read)
		}
		if _, ok := task.Outputs[id]; !ok {
			t.Errorf("build-tsc declares no %s port", id)
		}
	}
	for command, ids := range map[string][]string{"build": {"tsc", "tsc-auto", "transpile", "types", "compile"}, "test": {"tsc", "tsc-auto"}} {
		run := steps(readTscManifest(t), command)
		for _, id := range ids {
			if !slices.Contains(run[id].DependsOn, "^tsc") || !slices.Contains(run[id].DependsOn, "^tsc-auto") {
				t.Errorf("%s~%s depends on %v, want ^tsc and ^tsc-auto", command, id, run[id].DependsOn)
			}
		}
	}
}

// TestWorkloadStepsRunAfterTheTscSteps pins that a step which loads or
// bundles the project runs after the tsc steps of the project and of its
// dependencies: a package resolves a tsc library through its outDir, which
// only tsc writes or restores.
func TestWorkloadStepsRunAfterTheTscSteps(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-output-restores-at-outdir", "serve-run-and-package-run-after-the-tsc-steps")
	m := readTscManifest(t)
	for command, id := range map[string]string{"build": "compile", "serve": "serve", "run": "run", "package": "compile"} {
		step := steps(m, command)[id]
		for _, dep := range []string{"tsc", "tsc-auto", "^tsc", "^tsc-auto"} {
			if !slices.Contains(step.DependsOn, dep) {
				t.Errorf("%s~%s depends on %v, want %s", command, id, step.DependsOn, dep)
			}
		}
	}
}

// TestTscKeyReadsWhatTscReads pins the key of build-tsc and of the test
// steps that read a library's compiled output.
func TestTscKeyReadsWhatTscReads(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-key-follows-what-tsc-reads", "an-ignored-output-does-not-move-the-key")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-key-follows-what-tsc-reads", "a-dependency-tsconfig-change-moves-the-dependent-key")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-key-follows-what-tsc-reads", "a-dependent-test-runs-after-its-library-tsc")
	m := readTscManifest(t)
	inputs := m.Tasks["build-tsc"].Inputs
	// Only Git candidates: an ordinary pattern would also select the ignored
	// files tsc emits, and the build would key on its own output.
	sources := inputs["sources"]
	if sources.From != "project" || !slices.Equal(sources.Files, []string{"git:**/*", "!../**", "!node_modules/**", "!**/node_modules/**"}) {
		t.Errorf("build-tsc sources = %+v, want the project's Git candidates alone", sources)
	}
	closure := inputs["closureConfig"]
	if closure.From != "closure" || !slices.Contains(closure.Files, "*.json") || !slices.Contains(closure.Files, "**/tsconfig*.json") {
		t.Errorf("build-tsc closureConfig = %+v, want the closure's top-level JSON and tsconfig files", closure)
	}
	for _, file := range []string{"tsconfig*.json", "pnpm-lock.yaml", "package-lock.json", "yarn.lock", "bun.lock"} {
		if !slices.Contains(inputs["workspaceConfig"].Files, file) {
			t.Errorf("build-tsc workspaceConfig lacks %s", file)
		}
	}
	// The runtime and the target decide the node-target check after tsc.
	for _, param := range []string{"compiler", "runtime", "target"} {
		if inputs[param].From != "params" {
			t.Errorf("build-tsc does not key on the %s parameter", param)
		}
	}
	run := steps(m, "test")
	for _, id := range []string{"test", "types"} {
		if !slices.Contains(run[id].DependsOn, "^tsc") || !slices.Contains(run[id].DependsOn, "^tsc-auto") {
			t.Errorf("test~%s depends on %v, want its dependencies' tsc steps", id, run[id].DependsOn)
		}
	}
}

func TestPackageNpmRefusesATscProject(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "tsc-project-does-not-package", "package-refuses-a-tsc-project")
	root := t.TempDir()
	project := filepath.Join(root, "lib")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"lib","scripts":{"build":"tsc -b"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { out.WriteString(message + "\n") })
	status, _, err := runPackageNpm(&pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "lib", Path: "lib"}, Params: pctx.Params{}}, emit, nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("package-npm on a tsc project = %s, %v; want FAILED", status, err)
	}
	if !strings.Contains(out.String(), "compiles with tsc") {
		t.Errorf("package-npm reported no refusal of a tsc project:\n%s", out.String())
	}
}
