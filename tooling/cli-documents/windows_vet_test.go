package documents

import (
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
// nested go.mod. The project's test inputs are the whole tracked tree
// (options.test.filePatterns ["git:**"]), so every change selects this gate and
// changes its cache key.

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
