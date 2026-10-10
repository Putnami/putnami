package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// goFixtureVersion is the Go release the fixture workspace declares and pins.
const goFixtureVersion = "1.26.1"

// goCreateFixture is a workspace whose extension offers a Go template and runs
// its tasks with a toolchain locked as "go", which it installs inside the
// workspace, as @putnami/go installs the locked Go on a host without one.
type goCreateFixture struct {
	root string
	// pinned is where the extension installs the go the lock pins.
	pinned string
	// gos lists the go commands writeGo placed, in the order it placed them.
	gos *[]fixtureGo
}

// fixtureGo is a go command writeGo placed: the path it was given, the release
// it reports, and the file that records its runs.
type fixtureGo struct {
	path    string
	version string
	record  string
}

// newGoCreateFixture writes the workspace and points PATH at a directory
// without go. Its template does not render the Go framework version, so create
// asks no module proxy.
func newGoCreateFixture(t *testing.T) goCreateFixture {
	t.Helper()
	// The go commands are fixtureproc programs, built by the go on PATH before
	// the fixture takes it off PATH.
	fixtureproc.Prepare(t)
	root := t.TempDir()
	fixture := goCreateFixture{
		root:   root,
		pinned: filepath.Join(root, ".tools", "go-"+goFixtureVersion, "bin", "go"),
		gos:    &[]fixtureGo{},
	}
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"go-ws","includes":["ext"]}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@acme/go"}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.extension.json"), `{
  "name": "@acme/go",
  "version": "0.1.0",
  "cliContract": `+fmt.Sprint(protocolcli.CurrentContract)+`,
  "runtime": {
    "executable": "bin/runtime",
    "toolchains": {
      "compiler": {
        "lock": "go",
        "candidates": [
          {"from": "path", "path": "go"},
          {"from": "environment", "environment": "PUTNAMI_WORKSPACE_ROOT", "path": ".tools/go-{version}/bin/go"}
        ],
        "probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"},
        "prependPath": true
      }
    },
    "runToolchains": ["compiler"]
  }
}`)
	writeRawFile(t, filepath.Join(root, "ext", "templates", "go-app", "putnami.template.json"),
		`{"name":"go-app","description":"Go app","extension":"@acme/go"}`)
	writeRawFile(t, filepath.Join(root, "ext", "templates", "go-app", "go.mod.template"),
		"module <%= projectModule %>\n\ngo "+goFixtureVersion)
	writeRawFile(t, filepath.Join(root, "ext", "templates", "go-app", "putnami.json"), `{"name":"app"}`)
	if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)

	withoutGo := t.TempDir()
	t.Setenv("PATH", withoutGo)
	hometest.Temp(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	if path, err := osexec.LookPath("go"); err == nil {
		t.Fatalf("PATH still offers go at %s; this test guards nothing", path)
	}
	return fixture
}

// writeGo places a go command at path, with ".exe" appended on Windows, that
// reports version on every run, which is the answer to the probe `go env
// GOVERSION`, and records every run for goCalls. A second call for the same
// path and version keeps the command already there.
func (f goCreateFixture) writeGo(t *testing.T, path, version string) {
	t.Helper()
	f.writeGoAnswering(t, path, version, nil)
}

// writeGoAnswering is writeGo for a go command that answers the runs on names
// as their outcomes say. It launches the command once (fixtureproc.Warm), so
// the version probe that later starts it within its deadline does not also
// pay for the host's first-launch check of the new file.
func (f goCreateFixture) writeGoAnswering(t *testing.T, path, version string, on map[string]fixtureproc.Outcome) {
	t.Helper()
	for _, placed := range *f.gos {
		if placed.path != path {
			continue
		}
		if placed.version != version {
			t.Fatalf("writeGo: %s already reports go%s, not go%s", path, placed.version, version)
		}
		return
	}
	record := filepath.Join(f.root, "go-calls-"+strconv.Itoa(len(*f.gos))+".jsonl")
	fixtureproc.Warm(t, fixtureproc.Write(t, path, fixtureproc.Program{Record: record, Stdout: "go" + version + "\n", On: on}))
	*f.gos = append(*f.gos, fixtureGo{path: path, version: version, record: record})
}

// writeSlowGo places, where the extension installs the go the lock pins, a go
// command whose every run, the version probe included, outlasts the probe's
// deadline.
func (f goCreateFixture) writeSlowGo(t *testing.T) {
	t.Helper()
	fixtureproc.Warm(t, fixtureproc.Write(t, f.pinned, fixtureproc.Program{Stdout: "go" + goFixtureVersion + "\n", Sleep: time.Minute}))
}

// refusingGoInstall is the lock step and the workspace installers of a host
// whose go is installed: the test fails when the command runs either.
func (f goCreateFixture) refusingGoInstall(t *testing.T) LifecycleEnv {
	t.Helper()
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		t.Error("the command pinned a go although one is installed")
		return false, nil
	}
	return LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		t.Error("the command ran the workspace installers although a go is installed")
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}
}

// pin records the Go release in the workspace lock, as the lock step does.
func (f goCreateFixture) pin(t *testing.T) {
	t.Helper()
	locked := lockfile.NewLockFile()
	locked.SetToolchain("go", lockfile.LockEntry{
		Version:     goFixtureVersion,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): strings.Repeat("a", 64)},
		Source:      "https://go.dev/dl/",
	})
	if err := lockfile.WriteLockFile(f.root, locked); err != nil {
		t.Fatal(err)
	}
}

// goCalls returns every run of the placed go commands except the version probe,
// as "<args>|<GOTOOLCHAIN>|<path given to writeGo>". The runs of one command
// keep their order; the commands follow in the order writeGo placed them.
func (f goCreateFixture) goCalls(t *testing.T) []string {
	t.Helper()
	var calls []string
	for _, placed := range *f.gos {
		for _, run := range fixtureproc.Runs(t, placed.record) {
			if len(run.Args) >= 2 && run.Args[0] == "env" && run.Args[1] == "GOVERSION" {
				continue
			}
			toolchain, _ := run.LookupEnv("GOTOOLCHAIN")
			calls = append(calls, strings.Join(run.Args, " ")+"|"+toolchain+"|"+placed.path)
		}
	}
	return calls
}

func (f goCreateFixture) create(t *testing.T, env LifecycleEnv) (string, error) {
	t.Helper()
	return captureStdout(t, func() error {
		return ProjectsCreate(context.Background(), f.root, wsproto.Load(f.root), []string{"app", "--template", "go-app"}, false, env)
	})
}

// A consumer who installed only Putnami creates a Go project in a workspace
// whose lock pins no Go yet: create writes go.work, pins the Go it declares,
// runs the workspace installers that install that Go, and sets the project up
// with it.
func TestProjectsCreateInstallsThePinnedGoOnAHostWithoutGo(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go", "create-pins-and-installs-go-before-it-sets-the-project-up")
	var steps []string
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(_ context.Context, root string) (bool, error) {
		data, err := os.ReadFile(filepath.Join(root, "go.work"))
		if err != nil {
			t.Errorf("the lock step ran before go.work existed: %v", err)
		}
		if got, want := string(data), "go "+goFixtureVersion+"\n\nuse ./app\n"; got != want {
			t.Errorf("go.work = %q, want the bytes the go command writes, %q", got, want)
		}
		steps = append(steps, "pin")
		f.pin(t)
		return true, nil
	}
	env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		steps = append(steps, req.Job)
		if locked, err := lockfile.ReadLockFile(f.root); err != nil || locked == nil {
			t.Errorf("workspace-install ran without a lock: %v", err)
		} else if _, pinned := locked.GetToolchain("go"); !pinned {
			t.Error("workspace-install ran before the lock pinned go")
		}
		f.writeGo(t, f.pinned, goFixtureVersion)
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}

	output, err := f.create(t, env)
	if err != nil {
		t.Fatalf("projects create on a host without go: %v\n%s", err, output)
	}
	if got := strings.Join(steps, ","); got != "pin,workspace-install" {
		t.Fatalf("steps = %s, want the pin, then the installers", got)
	}
	want := []string{
		"work use ./app|local|" + f.pinned,
		"work edit -go=" + goFixtureVersion + "|local|" + f.pinned,
		"mod edit -go=" + goFixtureVersion + "|local|" + f.pinned,
		"mod tidy|local|" + f.pinned,
	}
	if got := f.goCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("go calls =\n%s\nwant, with the installed go:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(output, "Initialized go.work") || !strings.Contains(output, "Added app to go.work") {
		t.Fatalf("output does not report the go.work setup:\n%s", output)
	}
}

// The installers that install the pinned Go also install the workspace's
// dependencies. When they fail but still leave a go, create finishes the Go
// setup with it, keeps the project, and exits non-zero naming the command that
// finishes the install, as init --project does. It does not run the installers
// a second time.
func TestProjectsCreateNamesDepsInstallWhenTheGoInstallFails(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-reports-a-failed-install", "create-keeps-the-project-and-names-deps-install-when-the-go-install-fails")
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		f.pin(t)
		return true, nil
	}
	var jobs []string
	env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		jobs = append(jobs, req.Job)
		f.writeGo(t, f.pinned, goFixtureVersion)
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if err == nil {
		t.Fatalf("projects create succeeded although workspace-install failed:\n%s", output)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami deps install"; got != want {
		t.Fatalf("next command = %q, want %q (error %v)", got, want, err)
	}
	if !strings.Contains(err.Error(), "app") {
		t.Errorf("error = %v, want it to name the project", err)
	}
	if got := strings.Join(jobs, ","); got != "workspace-install" {
		t.Fatalf("workspace jobs = %s, want the one install that installed go", got)
	}
	if calls := f.goCalls(t); len(calls) != 4 || calls[len(calls)-1] != "mod tidy|local|"+f.pinned {
		t.Fatalf("go calls = %q, want the go.work setup and tidy with the installed go", calls)
	}
	if _, err := os.Stat(filepath.Join(f.root, "app", "go.mod")); err != nil {
		t.Fatalf("create removed the project: %v", err)
	}
	if includes := wsproto.Load(f.root).Includes; !slices.Contains(includes, "app") {
		t.Errorf("workspace includes = %v, want app kept", includes)
	}
}

// A go mod tidy that fails leaves a project that does not build. Create keeps
// the project, and fails naming tidy, quoting the end of go's output without
// the credentials a proxy URL carries, and naming the create command to run
// again.
func TestProjectsCreateFailsWhenGoModTidyFails(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-fails-when-go-mod-tidy-fails", "create-keeps-the-project-and-quotes-tidy-when-go-mod-tidy-fails")
	f.pin(t)
	var tidy strings.Builder
	tidy.WriteString("go: downloading example.com/first v1.0.0\n")
	for i := range goOutputMaxLines {
		fmt.Fprintf(&tidy, "go: downloading example.com/dep%d v1.0.0\n", i)
	}
	tidy.WriteString("go: app imports\n\tgo.putnami.dev/app: reading https://ci:s3cret@proxy.example.com/go.putnami.dev/app/@v/v0.7.3.zip: 404 Not Found\n")
	f.writeGoAnswering(t, f.pinned, goFixtureVersion, map[string]fixtureproc.Outcome{
		"mod tidy": {Stderr: tidy.String(), Exit: 1},
	})
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		t.Error("create ran the workspace installers although the pinned go is installed")
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if err == nil {
		t.Fatalf("projects create succeeded although go mod tidy failed:\n%s", output)
	}
	if !errors.Is(err, errGoModTidy) {
		t.Fatalf("error = %v, want %v", err, errGoModTidy)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami projects create app --template go-app --force"; got != want {
		t.Fatalf("next command = %q, want %q (error %v)", got, want, err)
	}
	message := err.Error()
	for _, want := range []string{"go mod tidy failed in app", "go.putnami.dev/app: reading https://[redacted]@proxy.example.com/"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to name %q", message, want)
		}
	}
	for _, leaked := range []string{"s3cret", "ci:", "example.com/first"} {
		if strings.Contains(message, leaked) {
			t.Errorf("error = %q, want no %q: credentials are redacted and only the end of the output is quoted", message, leaked)
		}
	}
	if calls := f.goCalls(t); len(calls) != 5 || calls[len(calls)-1] != "mod tidy|local|"+f.pinned {
		t.Fatalf("go calls = %q, want the go.work setup, then tidy", calls)
	}
	if _, err := os.Stat(filepath.Join(f.root, "app", "go.mod")); err != nil {
		t.Fatalf("create removed the project: %v", err)
	}
	if includes := wsproto.Load(f.root).Includes; !slices.Contains(includes, "app") {
		t.Errorf("workspace includes = %v, want app kept", includes)
	}
}

// When the installers that installed go failed and tidy fails after them,
// the error names both failures.
func TestProjectsCreateNamesTheInstallFailureWhenGoModTidyFailsAfterIt(t *testing.T) {
	f := newGoCreateFixture(t)
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		f.pin(t)
		return true, nil
	}
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		f.writeGoAnswering(t, f.pinned, goFixtureVersion, map[string]fixtureproc.Outcome{
			"mod tidy": {Stderr: "go: example.com/dep: 403 Forbidden\n", Exit: 1},
		})
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if !errors.Is(err, errGoModTidy) {
		t.Fatalf("error = %v, want %v\n%s", err, errGoModTidy, output)
	}
	for _, want := range []string{"example.com/dep: 403 Forbidden", "install workspace dependencies", "deps install failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami projects create app --template go-app --force"; got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

// The quoted output is the end of what go printed, one indented line per
// line, at most goOutputMaxLines lines and goOutputMaxBytes bytes.
func TestGoOutputExcerptKeepsTheBoundedEnd(t *testing.T) {
	if got := goOutputExcerpt([]byte(" \r\n\n")); got != "" {
		t.Errorf("excerpt of blank output = %q, want none", got)
	}
	if got, want := goOutputExcerpt([]byte("go: first\r\ngo: second\r\n")), "\n    go: first\n    go: second"; got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
	long := strings.Repeat("a", goOutputMaxBytes) + "é" + strings.Repeat("z", 10)
	got := goOutputExcerpt([]byte(long))
	if !strings.HasSuffix(got, "é"+strings.Repeat("z", 10)) || len(strings.TrimPrefix(got, "\n    ")) > goOutputMaxBytes {
		t.Errorf("excerpt of a %d-byte line = %d bytes ending %q, want the last %d bytes", len(long), len(got), got[len(got)-12:], goOutputMaxBytes)
	}
}

// When the installers provide no go either, create stops, names what failed,
// and names the command that creates the project again.
func TestProjectsCreateNamesTheRetryWhenNoGoCanBeInstalled(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go", "create-names-the-retry-when-no-go-results")
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		return false, errors.New("go.dev is unreachable")
	}
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if !errors.Is(err, errGoUnavailable) {
		t.Fatalf("error = %v, want %v\n%s", err, errGoUnavailable, output)
	}
	for _, reason := range []string{"go.dev is unreachable", "deps install failed", `workspace lock has no exact "go" pin`} {
		if !strings.Contains(err.Error(), reason) {
			t.Errorf("error = %v, want it to name %q", err, reason)
		}
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami projects create app --template go-app --force"; got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

// A go whose version probe does not exit within its deadline is there and
// slow, not missing: create installs nothing, stops naming the timeout, and
// names the command that creates the project again. The probe runs under its
// production deadline of 5 s.
func TestProjectsCreateReportsAGoProbeThatTimedOut(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go", "create-reports-a-go-probe-that-timed-out")
	f.pin(t)
	f.writeSlowGo(t)

	output, err := f.create(t, f.refusingGoInstall(t))
	if !errors.Is(err, jobs.ErrToolchainProbeTimeout) || errors.Is(err, errGoUnavailable) {
		t.Fatalf("error = %v, want the probe timeout, not %v\n%s", err, errGoUnavailable, output)
	}
	if want := "probe timed out after 5s"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name %q", err, want)
	}
	if strings.Contains(output, "No go command found") {
		t.Errorf("create reported a missing go:\n%s", output)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami projects create app --template go-app --force"; got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(f.root, "app", "go.mod")); err != nil {
		t.Errorf("create removed the project: %v", err)
	}
}

// A go the lock pins is found where the extension installed it, ahead of a go
// on PATH that runs another release, and nothing is installed again.
func TestProjectsCreateRunsThePinnedGoBeforeAnotherOnPath(t *testing.T) {
	f := newGoCreateFixture(t)
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go", "create-runs-the-pinned-go-before-another-on-path")
	f.pin(t)
	f.writeGo(t, f.pinned, goFixtureVersion)
	ambient := t.TempDir()
	f.writeGo(t, filepath.Join(ambient, "go"), "1.26.0")
	t.Setenv("PATH", ambient)
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		t.Error("create ran the workspace installers although the pinned go is installed")
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if err != nil {
		t.Fatalf("projects create: %v\n%s", err, output)
	}
	calls := f.goCalls(t)
	if len(calls) != 5 || calls[0] != "work init|local|"+f.pinned {
		t.Fatalf("go calls = %q, want the five setup calls, go.work created first", calls)
	}
	for _, call := range calls {
		if !strings.HasSuffix(call, "|"+f.pinned) {
			t.Fatalf("go call %q ran another go than the pinned %s", call, f.pinned)
		}
	}
}

// Without a usable pin, a go on PATH still sets the project up, as it did
// before tasks resolved their toolchains from the lock.
func TestProjectsCreateFallsBackToTheGoOnPath(t *testing.T) {
	f := newGoCreateFixture(t)
	ambient := t.TempDir()
	onPath := filepath.Join(ambient, "go")
	f.writeGo(t, onPath, "1.26.0")
	t.Setenv("PATH", ambient)
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		t.Error("create ran the workspace installers although PATH offers go")
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	output, err := f.create(t, env)
	if err != nil {
		t.Fatalf("projects create: %v\n%s", err, output)
	}
	calls := f.goCalls(t)
	if len(calls) != 5 || calls[0] != "work init|local|"+onPath {
		t.Fatalf("go calls = %q, want the five setup calls on %s", calls, onPath)
	}
}
