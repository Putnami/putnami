package windowsvet

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Windows compile gate (decision D-W11).
//
// Consumers run Putnami on windows/amd64, while contributors and this gate run
// on macOS and Linux. A Unix-only symbol such as syscall.Kill or a Setpgid
// field compiles here and breaks the Windows build. This test type-checks every
// go.work module, test files included, for windows/amd64 with `go vet`, which
// needs no Windows host.
//
// It names the modules through the go command's `work` pattern, which matches
// every package of every go.work module. One root `./...` would stop at each
// nested go.mod. The generated clients are not in Git: the project depends on
// every Go module that generates one, so the planner runs each client-generate
// before this gate.
//
// The gate reads every Go source of the repository. A file pattern that keys
// those sources selects this project, and the client generation it depends on,
// for every Go change. The project declares none: the gate runs only where
// wholeTreeGatesEnv is set, on main, and that run uses no cache.

// wholeTreeGatesEnv names the variable that turns the gate on. The manifest
// declares it under options.test.envInputs, so a run with it set never shares a
// cache key with a run without it.
const wholeTreeGatesEnv = "PUTNAMI_WHOLE_TREE_GATES"

// wholeTreeGatesWorkflowPath is the workflow that runs the gate on every push
// to main, from the repository root.
const wholeTreeGatesWorkflowPath = ".github/workflows/whole-tree-gates.yml"

// requireWholeTreeGates skips a test that reads the whole workspace unless
// wholeTreeGatesEnv is set.
func requireWholeTreeGates(t *testing.T) {
	t.Helper()
	if os.Getenv(wholeTreeGatesEnv) == "" {
		t.Skipf("%s is not set: this gate reads every go.work module and runs on main (%s). "+
			"Run it here with %s=1 ./putnamiw test --projects @putnami/windows-vet --no-cache.",
			wholeTreeGatesEnv, wholeTreeGatesWorkflowPath, wholeTreeGatesEnv)
	}
}

// windowsVetPlatform is the platform the gate compiles for: the only Windows
// target Putnami supports (D-W1).
var windowsVetPlatform = []string{"GOOS=windows", "GOARCH=amd64"}

func TestEveryGoWorkModuleVetsForWindows(t *testing.T) {
	requireWholeTreeGates(t)
	root := repositoryRoot(t)
	env := windowsVetEnv(root)

	modules := goOutputLines(t, root, env, "list", "-m", "-f", "{{.Path}}")
	if len(modules) == 0 {
		t.Fatal("go list -m named no go.work module, so the gate would check nothing")
	}
	covered := map[string]bool{}
	for _, module := range goOutputLines(t, root, env, "list", "-f", "{{.Module.Path}}", "work") {
		covered[module] = true
	}
	for _, module := range modules {
		if !covered[module] {
			t.Errorf("go.work module %s has no package that builds for windows/amd64, so vet checks nothing in it", module)
		}
	}

	cmd := exec.Command("go", "vet", "work")
	cmd.Dir = root
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s go vet work: %v\n%s\nMove the Unix-only code behind a _unix.go file or a //go:build unix constraint, next to a Windows counterpart.",
			strings.Join(windowsVetPlatform, " "), err, output)
	}
}

// TestTheGateDependsOnEveryGoModuleThatGeneratesAClient keeps the project's
// dependencies equal to the go.work modules that generate a client. Git holds
// no generated client, so `go vet work` type-checks one only when the planner
// ran its client-generate first, and the planner does so for a dependency. A
// module generates a client when its putnami.json declares a Go generatedFrom
// or the clientgen extension.
func TestTheGateDependsOnEveryGoModuleThatGeneratesAClient(t *testing.T) {
	requireWholeTreeGates(t)
	root := repositoryRoot(t)
	want := map[string]bool{}
	for _, dir := range goOutputLines(t, root, windowsVetEnv(root), "list", "-m", "-f", "{{.Dir}}") {
		config, ok := readProjectConfig(t, filepath.Join(dir, "putnami.json"))
		if !ok || !config.generatesGoClient() {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("module %s is outside the repository: %v", dir, err)
		}
		want["/"+filepath.ToSlash(rel)] = true
	}
	own, ok := readProjectConfig(t, "putnami.json")
	if !ok {
		t.Fatal("the gate's own putnami.json is missing")
	}
	got := map[string]bool{}
	for _, dependency := range own.Dependencies {
		got[dependency] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("%s generates a Go client, so add it to the dependencies of tooling/windows-vet/putnami.json", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("%s generates no Go client, so remove it from the dependencies of tooling/windows-vet/putnami.json", id)
		}
	}
}

// TestTheGateRunsOnMainWithoutACache keeps the two halves of the switch
// together: the manifest declares the variable as a key input and no file
// pattern that reaches another project, and the workflow sets the variable on
// a push to main and runs the gate without a cache. Without the first half a
// run with the variable restores the skip a run without it stored; without the
// second the gate runs nowhere, or replays a verdict for another tree.
func TestTheGateRunsOnMainWithoutACache(t *testing.T) {
	data, err := os.ReadFile("putnami.json")
	if err != nil {
		t.Fatalf("read the gate's own putnami.json: %v", err)
	}
	var manifest struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
				EnvInputs    []string `json:"envInputs"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse the gate's own putnami.json: %v", err)
	}
	declaresVariable, declaresWorkflow := false, false
	for _, name := range manifest.Options.Test.EnvInputs {
		declaresVariable = declaresVariable || name == wholeTreeGatesEnv
	}
	workflowPattern := "../../" + wholeTreeGatesWorkflowPath
	for _, pattern := range manifest.Options.Test.FilePatterns {
		if pattern == workflowPattern {
			declaresWorkflow = true
			continue
		}
		t.Errorf("options.test.filePatterns declares %q: a pattern over the workspace selects this gate, "+
			"and the client generation it depends on, for changes the tests that always run never read", pattern)
	}
	if !declaresVariable {
		t.Errorf("putnami.json does not declare %s under options.test.envInputs", wholeTreeGatesEnv)
	}
	if !declaresWorkflow {
		t.Errorf("putnami.json does not declare %s under options.test.filePatterns, though this test reads it", workflowPattern)
	}

	workflow, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(wholeTreeGatesWorkflowPath)))
	if err != nil {
		t.Fatalf("read the workflow that runs the gate on main: %v", err)
	}
	var pushesToMain, setsVariable, runsUncached bool
	inPush := false
	for _, raw := range strings.Split(string(workflow), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		switch {
		case indent == 2 && line == "push:":
			inPush = true
		case indent <= 2:
			inPush = false
		case inPush && line == "branches: [main]":
			pushesToMain = true
		}
		if value, ok := strings.CutPrefix(line, wholeTreeGatesEnv+":"); ok && strings.TrimSpace(value) != "''" {
			setsVariable = true
		}
		if strings.Contains(line, "./putnamiw test ") && strings.Contains(line, "@putnami/windows-vet") && strings.Contains(line, " --no-cache") {
			runsUncached = true
		}
	}
	if !pushesToMain {
		t.Errorf("%s does not run on a push to main (on.push.branches: [main])", wholeTreeGatesWorkflowPath)
	}
	if !setsVariable {
		t.Errorf("%s does not set %s", wholeTreeGatesWorkflowPath, wholeTreeGatesEnv)
	}
	if !runsUncached {
		t.Errorf("%s has no step that runs ./putnamiw test over @putnami/windows-vet with --no-cache", wholeTreeGatesWorkflowPath)
	}
}

// projectConfig is the part of a putnami.json that says whether a project
// generates a Go client.
type projectConfig struct {
	Dependencies  []string `json:"dependencies"`
	Extensions    []string `json:"extensions"`
	GeneratedFrom *struct {
		Language string `json:"language"`
	} `json:"generatedFrom"`
}

func (c projectConfig) generatesGoClient() bool {
	if c.GeneratedFrom != nil && c.GeneratedFrom.Language == "go" {
		return true
	}
	for _, extension := range c.Extensions {
		if extension == "/tooling/clientgen-extension" {
			return true
		}
	}
	return false
}

// readProjectConfig reads one putnami.json, reporting false when the file does
// not exist.
func readProjectConfig(t *testing.T, file string) (projectConfig, bool) {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return projectConfig{}, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var config projectConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return config, true
}

// windowsVetEnv is the caller's environment pinned to the repository's go.work
// and to windows/amd64. cgo is off, as it is for any cross-compilation without
// a C toolchain, and GOFLAGS is cleared so that an ambient flag such as -race
// cannot change what the gate checks. os/exec keeps the last value of a
// repeated variable, so these entries win.
func windowsVetEnv(root string) []string {
	env := append(os.Environ(), windowsVetPlatform...)
	return append(env,
		"CGO_ENABLED=0",
		"GOFLAGS=",
		"GOWORK="+filepath.Join(root, "go.work"),
	)
}

// goOutputLines runs the go command in root and returns its distinct non-empty
// output lines.
func goOutputLines(t *testing.T, root string, env []string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	seen := map[string]bool{}
	lines := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return lines
}

// repositoryRoot walks up from the test's working directory, the project root,
// to the directory holding putnami.workspace.json.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json above the test working directory")
		}
		dir = parent
	}
}
