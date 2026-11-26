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
// nested go.mod. The generated clients are not in Git: the workflow that runs
// this gate generates them first (generateClientsCommand).
//
// The gate reads every Go source of the repository. A file pattern that keys
// those sources selects this project for every Go change. The project declares
// none: the gate runs only where wholeTreeGatesEnv is set, on main, and that
// run uses no cache.

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
		t.Fatalf("%s go vet work: %v\n%s\nA missing generated client means a generation step of "+wholeTreeGatesWorkflowPath+" did not run. Otherwise move the Unix-only code behind a _unix.go file or a //go:build unix constraint, next to a Windows counterpart.",
			strings.Join(windowsVetPlatform, " "), err, output)
	}
}

// TestTheWorkflowGeneratesEveryGoClient keeps the workflow's generation steps
// equal to the go.work modules that generate a client. Git holds no generated
// client, so `go vet work` type-checks one only when a step generated it. A
// module generates a client when its putnami.json declares a Go generatedFrom
// or the clientgen extension. `client-generate --all` covers it unless it
// carries a tag the workspace disables; the workflow must then name it in a
// `client-generate --projects` step.
func TestTheWorkflowGeneratesEveryGoClient(t *testing.T) {
	requireWholeTreeGates(t)
	root := repositoryRoot(t)
	var workspace struct {
		Disable struct {
			Tags []string `json:"tags"`
		} `json:"disable"`
	}
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if err != nil {
		t.Fatalf("read putnami.workspace.json: %v", err)
	}
	if err := json.Unmarshal(data, &workspace); err != nil {
		t.Fatalf("parse putnami.workspace.json: %v", err)
	}
	disabled := map[string]bool{}
	for _, tag := range workspace.Disable.Tags {
		disabled[tag] = true
	}
	workflow, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wholeTreeGatesWorkflowPath)))
	if err != nil {
		t.Fatalf("read the workflow that runs the gate on main: %v", err)
	}
	named := map[string]bool{}
	for _, raw := range strings.Split(string(workflow), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if _, ids, ok := strings.Cut(line, generateNamedClientsCommand); ok {
			for _, id := range strings.Split(strings.TrimSpace(ids), ",") {
				named[id] = true
			}
		}
	}
	want := map[string]bool{}
	for _, dir := range goOutputLines(t, root, windowsVetEnv(root), "list", "-m", "-f", "{{.Dir}}") {
		config, ok := readProjectConfig(t, filepath.Join(dir, "putnami.json"))
		if !ok || !config.generatesGoClient() {
			continue
		}
		skippedByAll := false
		for _, tag := range config.Tags {
			skippedByAll = skippedByAll || disabled[tag]
		}
		if !skippedByAll {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("module %s is outside the repository: %v", dir, err)
		}
		id := "/" + filepath.ToSlash(rel)
		want[id] = true
		if !named[id] {
			t.Errorf("%s generates a Go client and carries a tag the workspace disables, so %q skips it: "+
				"name it in a %q step of %s", id, generateClientsCommand, generateNamedClientsCommand+"<ids>", wholeTreeGatesWorkflowPath)
		}
	}
	for id := range named {
		if !want[id] {
			t.Errorf("%s names %s in a %q step, but %q already covers it or it generates no Go client: remove it",
				wholeTreeGatesWorkflowPath, id, generateNamedClientsCommand+"<ids>", generateClientsCommand)
		}
	}
}

// projectConfig is the part of a putnami.json that says whether a project
// generates a Go client, and which tags it carries.
type projectConfig struct {
	Tags          []string `json:"tags"`
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

// TestTheGateRunsOnMainWithoutACache keeps the halves of the switch together.
// The manifest declares the variable as a key input, no file pattern that
// reaches another project and no dependency: each of the last two selects this
// project for changes its always-run tests never read. The workflow sets the
// variable on a push to main, generates the clients Git does not hold, and
// runs the gate without a cache. Without the declaration a run with the
// variable restores the skip a run without it stored; without the workflow the
// gate runs nowhere, or replays a verdict for another tree.
func TestTheGateRunsOnMainWithoutACache(t *testing.T) {
	data, err := os.ReadFile("putnami.json")
	if err != nil {
		t.Fatalf("read the gate's own putnami.json: %v", err)
	}
	var manifest struct {
		Dependencies []string `json:"dependencies"`
		Options      struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
				EnvInputs    []string `json:"envInputs"`
				Count        int      `json:"count"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse the gate's own putnami.json: %v", err)
	}
	if manifest.Options.Test.Count != 1 {
		t.Errorf("options.test.count = %d, want 1: go test replays a stored pass when no file it tracks moved, "+
			"and it tracks neither the generated clients nor the modules the gate vets", manifest.Options.Test.Count)
	}
	for _, dependency := range manifest.Dependencies {
		t.Errorf("putnami.json depends on %s: a change that reaches it plans this gate and every other "+
			"dependency's steps, then skips the gate. The workflow generates the clients instead.", dependency)
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
		t.Errorf("options.test.filePatterns declares %q: a pattern over the workspace selects this gate "+
			"for changes the tests that always run never read", pattern)
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
	for _, problem := range workflowProblems(string(workflow)) {
		t.Errorf("%s %s", wholeTreeGatesWorkflowPath, problem)
	}
}

// TestWorkflowProblemsNameEachMissingPart keeps the workflow check from passing
// on a workflow that lost its trigger, its variable, its client generation or
// its cache switch.
func TestWorkflowProblemsNameEachMissingPart(t *testing.T) {
	complete := "on:\n  push:\n    branches: [main]\njobs:\n  gates:\n    env:\n      " + wholeTreeGatesEnv + ": '1'\n    steps:\n" +
		"      - run: " + generateClientsCommand + "\n" +
		"      - run: ./putnamiw test --projects @putnami/cli-documents,@putnami/windows-vet --no-cache\n"
	if problems := workflowProblems(complete); len(problems) != 0 {
		t.Fatalf("a complete workflow reports %v", problems)
	}
	for name, broken := range map[string]string{
		"no push to main":   strings.Replace(complete, "branches: [main]", "branches: [release]", 1),
		"no variable":       strings.Replace(complete, wholeTreeGatesEnv, "ANOTHER_VARIABLE", 1),
		"empty variable":    strings.Replace(complete, ": '1'", ": ''", 1),
		"no generation":     strings.Replace(complete, "      - run: "+generateClientsCommand+"\n", "", 1),
		"generation last":   strings.Replace(strings.Replace(complete, "      - run: "+generateClientsCommand+"\n", "", 1), "--no-cache\n", "--no-cache\n      - run: "+generateClientsCommand+"\n", 1),
		"cached":            strings.Replace(complete, " --no-cache", "", 1),
		"another project":   strings.Replace(complete, ",@putnami/windows-vet", "", 1),
		"gate commented":    strings.Replace(complete, "      - run: ./putnamiw test", "      # - run: ./putnamiw test", 1),
		"trigger commented": strings.Replace(complete, "    branches: [main]", "    # branches: [main]", 1),
	} {
		if problems := workflowProblems(broken); len(problems) != 1 {
			t.Errorf("%s: problems = %v, want exactly one", name, problems)
		}
	}
}

// generateClientsCommand generates every client of the workspace. Git holds no
// generated client, and `go vet work` type-checks the modules that import one,
// so the workflow runs it before the gate.
const generateClientsCommand = "./putnamiw client-generate --all"

// generateNamedClientsCommand prefixes the ids of the modules that command
// skips because they carry a tag the workspace disables.
const generateNamedClientsCommand = "./putnamiw client-generate --projects "

// workflowProblems reports what a workflow lacks to run the gate on main: a
// push trigger on main, the variable that turns the gate on, the generation of
// the clients, and after it a test command over this project that uses no
// cache. It reads lines, not YAML: comment lines are dropped, and each part
// must stand on one line.
func workflowProblems(workflow string) []string {
	var pushesToMain, setsVariable, generatesClients, runsUncached, runsAfterGeneration bool
	inPush := false
	for _, raw := range strings.Split(workflow, "\n") {
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
		if strings.HasSuffix(line, generateClientsCommand) {
			generatesClients = true
		}
		if strings.Contains(line, "./putnamiw test ") && strings.Contains(line, "@putnami/windows-vet") && strings.Contains(line, " --no-cache") {
			runsUncached = true
			runsAfterGeneration = generatesClients
		}
	}
	var problems []string
	if !pushesToMain {
		problems = append(problems, "does not run on a push to main (on.push.branches: [main])")
	}
	if !setsVariable {
		problems = append(problems, "does not set "+wholeTreeGatesEnv)
	}
	if !generatesClients {
		problems = append(problems, "has no step that runs "+generateClientsCommand)
	}
	if !runsUncached {
		problems = append(problems, "has no step that runs ./putnamiw test over @putnami/windows-vet with --no-cache")
	}
	if generatesClients && runsUncached && !runsAfterGeneration {
		problems = append(problems, "runs the gate before the client generation")
	}
	return problems
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
