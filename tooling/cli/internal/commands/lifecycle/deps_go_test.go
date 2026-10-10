package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// withGoProject adds a Go project to the fixture workspace, as `projects
// create` leaves it: a member of the configuration and of go.work.
func (f goCreateFixture) withGoProject(t *testing.T) {
	t.Helper()
	writeRawFile(t, filepath.Join(f.root, "putnami.workspace.json"), `{"name":"go-ws","includes":["app","ext"]}`)
	writeRawFile(t, filepath.Join(f.root, "app", "putnami.json"), `{"name":"app"}`)
	writeRawFile(t, filepath.Join(f.root, "app", "go.mod"), "module example.com/app\n\ngo "+goFixtureVersion+"\n")
	writeRawFile(t, filepath.Join(f.root, "go.work"), "go "+goFixtureVersion+"\n\nuse ./app\n")
	workspace.InvalidateLoadCache(f.root)
}

// depsCalls returns the go invocations as "<args>|<binary>".
func (f goCreateFixture) depsCalls(t *testing.T) []string {
	t.Helper()
	calls := f.goCalls(t)
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		fields := strings.Split(call, "|")
		out = append(out, fields[0]+"|"+fields[len(fields)-1])
	}
	return out
}

// `deps add` and `deps remove` run the go command a task of the workspace's Go
// extension runs, the release the workspace lock pins, and not a go on PATH:
// a host with only Putnami installed has none.
func TestDepsAddAndRemoveRunThePinnedGo(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-run-the-pinned-go", "deps-add-and-remove-run-the-pinned-go")
	f := newGoCreateFixture(t)
	f.withGoProject(t)
	f.pin(t)
	f.writeGo(t, f.pinned, goFixtureVersion)
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		t.Error("deps ran the workspace installers although the pinned go is installed")
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}

	if output, err := captureStdout(t, func() error {
		return DepsAdd(context.Background(), f.root, []string{"example.com/lib@v1.0.0"}, "", env)
	}); err != nil {
		t.Fatalf("deps add on a host without go: %v\n%s", err, output)
	}
	if output, err := captureStdout(t, func() error {
		return DepsRemove(context.Background(), f.root, []string{"example.com/lib@v1.0.0"}, "app", env)
	}); err != nil {
		t.Fatalf("deps remove on a host without go: %v\n%s", err, output)
	}
	want := []string{
		"get example.com/lib@v1.0.0|" + f.pinned,
		"mod tidy|" + f.pinned,
		"get example.com/lib@none|" + f.pinned,
		"mod tidy|" + f.pinned,
	}
	if got := f.depsCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("go calls =\n%s\nwant, with the pinned go:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// On a host where the pinned Go is not installed yet, `deps add` installs it
// the way `projects create` does, and then runs it.
func TestDepsAddInstallsThePinnedGoOnAHostWithoutGo(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-run-the-pinned-go", "deps-add-installs-the-pinned-go-first")
	f := newGoCreateFixture(t)
	f.withGoProject(t)
	goWork, err := os.ReadFile(filepath.Join(f.root, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		steps = append(steps, "pin")
		f.pin(t)
		return true, nil
	}
	env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		steps = append(steps, req.Job)
		f.writeGo(t, f.pinned, goFixtureVersion)
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}

	output, err := captureStdout(t, func() error {
		return DepsAdd(context.Background(), f.root, []string{"example.com/lib@v1.0.0"}, "", env)
	})
	if err != nil {
		t.Fatalf("deps add on a host without go: %v\n%s", err, output)
	}
	if got := strings.Join(steps, ","); got != "pin,workspace-install" {
		t.Fatalf("steps = %s, want the pin, then the installers", got)
	}
	want := []string{"get example.com/lib@v1.0.0|" + f.pinned, "mod tidy|" + f.pinned}
	if got := f.depsCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("go calls =\n%s\nwant, with the installed go:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if after, err := os.ReadFile(filepath.Join(f.root, "go.work")); err != nil || string(after) != string(goWork) {
		t.Fatalf("deps add rewrote go.work: %q, %v", after, err)
	}
}

// A go whose version probe does not exit within its deadline is there and
// slow, not missing: `deps add` installs nothing, stops naming the timeout,
// and names the same `deps add` as the command to run next.
func TestDepsAddReportsAGoProbeThatTimedOut(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-run-the-pinned-go", "deps-add-reports-a-go-probe-that-timed-out")
	f := newGoCreateFixture(t)
	f.withGoProject(t)
	f.pin(t)
	f.writeSlowGo(t)
	env := f.refusingGoInstall(t)

	ctx := jobs.WithToolchainProbeTimeout(context.Background(), testProbeTimeout)
	output, err := captureStdout(t, func() error {
		return DepsAdd(ctx, f.root, []string{"example.com/lib@v1.0.0"}, "", env)
	})
	if !errors.Is(err, jobs.ErrToolchainProbeTimeout) || errors.Is(err, errGoUnavailable) {
		t.Fatalf("error = %v, want the probe timeout, not %v\n%s", err, errGoUnavailable, output)
	}
	if want := "probe timed out after " + testProbeTimeout.String(); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name %q", err, want)
	}
	if strings.Contains(output, "No go command found") {
		t.Errorf("deps add reported a missing go:\n%s", output)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami deps add example.com/lib@v1.0.0"; got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

// The command a deps run names after a probe timeout is the one the user ran:
// its modules, except for prune, which takes none, and its project selector.
func TestDepsCommandIsTheCommandTheUserRan(t *testing.T) {
	for _, tc := range []struct {
		action, selector string
		modules          []string
		want             string
	}{
		{"add", "", []string{"example.com/lib@v1.0.0"}, "putnami deps add example.com/lib@v1.0.0"},
		{"remove", "api", []string{"example.com/a", "example.com/b"}, "putnami deps remove example.com/a example.com/b --projects api"},
		{"prune", "", nil, "putnami deps prune"},
		{"prune", "api", nil, "putnami deps prune --projects api"},
	} {
		if got := depsCommand(tc.action, tc.modules, tc.selector); got != tc.want {
			t.Errorf("depsCommand(%q, %q, %q) = %q, want %q", tc.action, tc.modules, tc.selector, got, tc.want)
		}
	}
}

// failingGoInstall is the workspace installers of a host without go that
// install the pinned Go and then fail on another dependency. jobs records the
// workspace jobs they run.
func (f goCreateFixture) failingGoInstall(t *testing.T, jobs *[]string) LifecycleEnv {
	t.Helper()
	origFill := fillImplicitToolchainPins
	t.Cleanup(func() { fillImplicitToolchainPins = origFill })
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) {
		f.pin(t)
		return true, nil
	}
	return LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		*jobs = append(*jobs, req.Job)
		f.writeGo(t, f.pinned, goFixtureVersion)
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}
}

// The installers that install the pinned Go also install the workspace's
// dependencies. When they fail but still leave a go, `deps add` and `deps
// remove` edit the module with it and exit non-zero naming the command that
// finishes the install, as `projects create` does.
func TestDepsAddAndRemoveNameDepsInstallWhenTheGoInstallFails(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-report-a-failed-install",
		"deps-add-and-remove-edit-and-name-deps-install-when-the-go-install-fails")
	for _, tc := range []struct {
		action string
		run    func(root string, env LifecycleEnv) error
		get    string
	}{
		{
			action: "add",
			run: func(root string, env LifecycleEnv) error {
				return DepsAdd(context.Background(), root, []string{"example.com/lib@v1.0.0"}, "", env)
			},
			get: "get example.com/lib@v1.0.0",
		},
		{
			action: "remove",
			run: func(root string, env LifecycleEnv) error {
				return DepsRemove(context.Background(), root, []string{"example.com/lib"}, "app", env)
			},
			get: "get example.com/lib@none",
		},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := newGoCreateFixture(t)
			f.withGoProject(t)
			var jobs []string
			env := f.failingGoInstall(t, &jobs)

			output, err := captureStdout(t, func() error { return tc.run(f.root, env) })
			if err == nil {
				t.Fatalf("deps %s succeeded although workspace-install failed:\n%s", tc.action, output)
			}
			if got, want := protocolcli.SuggestedNext(err), "putnami deps install"; got != want {
				t.Fatalf("next command = %q, want %q (error %v)", got, want, err)
			}
			if !strings.Contains(err.Error(), "deps "+tc.action) {
				t.Errorf("error = %v, want it to name deps %s", err, tc.action)
			}
			if got := strings.Join(jobs, ","); got != "workspace-install" {
				t.Fatalf("workspace jobs = %s, want the one install that installed go", got)
			}
			want := []string{tc.get + "|" + f.pinned, "mod tidy|" + f.pinned}
			if got := f.depsCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("go calls =\n%s\nwant the edit with the installed go:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// goPruneWorkspace writes two Go projects, app and tool, whose go.mod requires
// the workspace module example.com/lib, and returns the workspace model the
// declared-edge check reads: no import backs either requirement, so both are
// manifest findings that prune removes through `deps remove`. app's
// putnami.json also declares appConfigDeps, which no import backs: findings
// prune removes from that file.
func (f goCreateFixture) goPruneWorkspace(t *testing.T, appConfigDeps ...string) *workspace.Workspace {
	t.Helper()
	writeRawFile(t, filepath.Join(f.root, "putnami.workspace.json"), `{"name":"go-ws","includes":["app","ext","lib","tool"]}`)
	for _, name := range []string{"app", "tool"} {
		config := `{"name":"` + name + `"}`
		if name == "app" && len(appConfigDeps) > 0 {
			config = `{"name":"app","dependencies":["` + strings.Join(appConfigDeps, `","`) + `"]}`
		}
		writeRawFile(t, filepath.Join(f.root, name, "putnami.json"), config)
		writeRawFile(t, filepath.Join(f.root, name, "go.mod"),
			"module example.com/"+name+"\n\ngo "+goFixtureVersion+"\n\nrequire example.com/lib v0.0.0\n")
	}
	writeRawFile(t, filepath.Join(f.root, "lib", "putnami.json"), `{"name":"example.com/lib"}`)
	writeRawFile(t, filepath.Join(f.root, "lib", "go.mod"), "module example.com/lib\n\ngo "+goFixtureVersion+"\n")
	writeRawFile(t, filepath.Join(f.root, "go.work"), "go "+goFixtureVersion+"\n\nuse (\n\t./app\n\t./lib\n\t./tool\n)\n")
	workspace.InvalidateLoadCache(f.root)

	requiresLib := func(id, name string) *workspace.Project {
		return &workspace.Project{
			ID: id, Name: name, Path: name,
			Dependencies: []string{"example.com/lib"},
			DependencySources: map[string]wsproto.DependencySource{
				"example.com/lib": wsproto.DependencySourceDeclared,
			},
		}
	}
	app := requiresLib("/app", "app")
	if len(appConfigDeps) > 0 {
		app.Config = &wsproto.ProjectConfig{Name: "app", Dependencies: appConfigDeps}
		app.Dependencies = append(app.Dependencies, appConfigDeps...)
	}
	lib := &workspace.Project{ID: "/lib", Name: "example.com/lib", SourceName: "example.com/lib", Path: "lib"}
	return workspace.NewWorkspace(f.root, nil, []*workspace.Project{app, lib, requiresLib("/tool", "tool")})
}

// A go whose version probe times out stops `deps prune` before it edits any
// file: the putnami.json and go.mod files it would prune keep their bytes, so
// the same `deps prune` it names as the command to run next reads the provider
// view this run read.
func TestDepsPruneChangesNothingWhenTheGoProbeTimesOut(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-run-the-pinned-go", "deps-prune-changes-nothing-when-the-go-probe-times-out")
	f := newGoCreateFixture(t)
	ws := f.goPruneWorkspace(t, "tool")
	f.pin(t)
	f.writeSlowGo(t)
	env := f.refusingGoInstall(t)
	files := []string{
		filepath.Join("app", "putnami.json"), filepath.Join("app", "go.mod"),
		filepath.Join("tool", "putnami.json"), filepath.Join("tool", "go.mod"),
	}
	before := make(map[string]string, len(files))
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(f.root, file))
		if err != nil {
			t.Fatal(err)
		}
		before[file] = string(data)
	}

	ctx := jobs.WithToolchainProbeTimeout(context.Background(), testProbeTimeout)
	var out bytes.Buffer
	output, err := captureStdout(t, func() error {
		return prunePlan(ctx, f.root, ws, "", false, &out, env)
	})
	if !errors.Is(err, jobs.ErrToolchainProbeTimeout) || errors.Is(err, errGoUnavailable) {
		t.Fatalf("error = %v, want the probe timeout, not %v\n%s%s", err, errGoUnavailable, out.String(), output)
	}
	if !strings.Contains(out.String(), "prune app → tool [putnami-json]") {
		t.Fatalf("prune output = %q, want the putnami.json finding listed", out.String())
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami deps prune"; got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
	for _, file := range files {
		if data, err := os.ReadFile(filepath.Join(f.root, file)); err != nil || string(data) != before[file] {
			t.Errorf("deps prune changed %s after the probe timed out: %q, %v", file, data, err)
		}
	}
	if calls := f.depsCalls(t); len(calls) != 0 {
		t.Errorf("go calls = %q, want none", calls)
	}
}

// `deps prune` edits every Go module it selected with the go the failed
// installers left, runs them once, and exits non-zero naming the command that
// finishes the install.
func TestDepsPruneNamesDepsInstallWhenTheGoInstallFails(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-report-a-failed-install",
		"deps-prune-edits-every-module-and-names-deps-install-when-the-go-install-fails")
	f := newGoCreateFixture(t)
	ws := f.goPruneWorkspace(t)
	var jobs []string
	env := f.failingGoInstall(t, &jobs)

	var out bytes.Buffer
	output, err := captureStdout(t, func() error {
		return prunePlan(context.Background(), f.root, ws, "", false, &out, env)
	})
	if err == nil {
		t.Fatalf("deps prune succeeded although workspace-install failed:\n%s%s", out.String(), output)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami deps install"; got != want {
		t.Fatalf("next command = %q, want %q (error %v)", got, want, err)
	}
	if !strings.Contains(err.Error(), "deps prune") {
		t.Errorf("error = %v, want it to name deps prune", err)
	}
	if got := strings.Join(jobs, ","); got != "workspace-install" {
		t.Fatalf("workspace jobs = %s, want the one install that installed go", got)
	}
	want := []string{
		"mod edit -droprequire=example.com/lib|" + f.pinned,
		"mod tidy|" + f.pinned,
		"mod edit -droprequire=example.com/lib|" + f.pinned,
		"mod tidy|" + f.pinned,
	}
	if got := f.depsCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("go calls =\n%s\nwant both modules edited with the installed go:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "pruned 2 declared edge(s) across 2 project(s)") {
		t.Errorf("prune output = %q, want both projects pruned", out.String())
	}
}

// The edit prune runs drops a requirement that only go.work satisfies, with a
// real go and no module proxy: `require example.com/lib v0.0.0` of a workspace
// module that no proxy serves. `go get example.com/lib@none`, which prune ran
// before, resolves that version and fails on this layout.
func TestDepsPruneEditDropsARequirementOnlyGoWorkSatisfies(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.22\n")
	writeRawFile(t, filepath.Join(root, "lib", "lib.go"), "package lib\n")
	writeRawFile(t, filepath.Join(root, "api", "go.mod"), "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n")
	writeRawFile(t, filepath.Join(root, "api", "main.go"), "package main\n\nfunc main() {}\n")
	writeRawFile(t, filepath.Join(root, "go.work"), "go 1.22\n\nuse (\n\t./api\n\t./lib\n)\n")
	moduleDir := filepath.Join(root, "api")
	env := shared.GoCommandEnvFrom(append(os.Environ(), "GOPROXY=off", "GOFLAGS=", "GOTOOLCHAIN=local"), moduleDir)

	edit, _ := goDepsEdit("prune", []string{"example.com/lib"})
	for _, args := range [][]string{edit, {"mod", "tidy"}} {
		cmd := exec.Command(goBinary, args...)
		cmd.Dir = moduleDir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "example.com/lib") {
		t.Fatalf("api/go.mod still requires example.com/lib:\n%s", data)
	}
}
