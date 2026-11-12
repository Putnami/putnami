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
// nested go.mod. The project's test inputs are the tracked Go sources, go.mod
// and go.sum files of the repository, go.work, and the committed contracts the
// generated clients come from, so a change to any of them selects this gate and
// changes its cache key. The generated clients themselves are not in Git: the
// project depends on every Go module that generates one, so the planner runs
// each client-generate before this gate.

// windowsVetPlatform is the platform the gate compiles for: the only Windows
// target Putnami supports (D-W1).
var windowsVetPlatform = []string{"GOOS=windows", "GOARCH=amd64"}

func TestEveryGoWorkModuleVetsForWindows(t *testing.T) {
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
