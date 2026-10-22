package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchainpins"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// noProjectJob is the job runner of a workspace whose extensions declare no
// project job: no extension provides the job it is asked for.
func noProjectJob(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
	return WorkspaceJobResult{Outcome: WorkspaceJobMissing}, nil
}

// jobRecorder is a job runner that records every request and answers each
// with outcome.
type jobRecorder struct {
	outcome  WorkspaceJobOutcome
	mu       sync.Mutex
	requests []WorkspaceJobRequest
}

func (r *jobRecorder) run(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return WorkspaceJobResult{Outcome: r.outcome}, nil
}

// asked returns the requests recorded so far.
func (r *jobRecorder) asked() []WorkspaceJobRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WorkspaceJobRequest(nil), r.requests...)
}

// paramsJSON is the JSON document of a job request's params, the bytes the job
// context hands the extension.
func paramsJSON(t *testing.T, req WorkspaceJobRequest) string {
	t.Helper()
	data, err := json.Marshal(req.Params)
	if err != nil {
		t.Fatalf("encode the params of %s: %v", req.Job, err)
	}
	return string(data)
}

// starterWorkspace writes a workspace with an empty lock and the template
// starter of the extension @acme/app in its acme domain.
func starterWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"starter-ws"}`)
	if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "acme", "templates", "starter")
	writeRawFile(t, filepath.Join(template, "putnami.template.json"),
		`{"name":"starter","description":"Starter","extension":"@acme/app","version":"0.1.0"}`)
	writeRawFile(t, filepath.Join(template, "putnami.json.template"), `{"name":"<%= projectName %>"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// projectIDAt is the ID of the project the workspace at root lists at path.
func projectIDAt(t *testing.T, root, path string) string {
	t.Helper()
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	project := projectAtPath(ws, path)
	if project == nil {
		t.Fatalf("the workspace lists no project at %s", path)
	}
	return project.ID
}

// Create renders the template, then runs the project-setup job for the new
// project only, with the project's path and, for the create of an init that
// chose a channel, that channel.
func TestCreateRunsProjectSetupWithThePathAndTheChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "project-setup-runs-in-the-project-extension",
		"create-runs-project-setup-with-the-path-and-the-channel")
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "project-setup-receives-the-channel")

	for _, channel := range []string{"", candidateChannel} {
		t.Run("channel "+channel, func(t *testing.T) {
			t.Parallel()
			root := starterWorkspace(t)
			recorder := &jobRecorder{outcome: WorkspaceJobOK}
			var out bytes.Buffer
			env := LifecycleEnv{Out: &out, RunJob: recorder.run, channel: channel}
			if err := createInitProjectFiles(context.Background(), root, wsproto.Load(root),
				[]string{"api", "--template", "starter"}, env); err != nil {
				t.Fatalf("create: %v\n%s", err, out.String())
			}
			asked := recorder.asked()
			if len(asked) != 1 {
				t.Fatalf("create ran %d jobs, want the project-setup job once: %+v", len(asked), asked)
			}
			want := `{"path":"api"}`
			if channel != "" {
				want = `{"path":"api","` + initChannelJobOption + `":"` + channel + `"}`
			}
			req := asked[0]
			if got := paramsJSON(t, req); req.Job != toolchain.ProjectSetupCommand || req.Projects != projectIDAt(t, root, "api") || got != want {
				t.Fatalf("create ran %s for %q with %s, want %s for the new project with %s",
					req.Job, req.Projects, got, toolchain.ProjectSetupCommand, want)
			}
		})
	}
}

// A failed project-setup keeps the rendered project, runs the job once, and
// names the create to run again with --force.
func TestCreateKeepsTheProjectWhenTheSetupFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "project-setup-runs-in-the-project-extension",
		"a-failed-setup-keeps-the-project-and-names-the-create-to-run-again")
	root := starterWorkspace(t)
	recorder := &jobRecorder{outcome: WorkspaceJobFailed}
	var out bytes.Buffer
	err := ProjectsCreate(context.Background(), root, wsproto.Load(root),
		[]string{"api", "--template", "starter"}, LifecycleEnv{Out: &out, RunJob: recorder.run})
	if err == nil || !strings.Contains(err.Error(), "set up project api") {
		t.Fatalf("create = %v, want the failed setup of api", err)
	}
	if next := protocolcli.SuggestedNext(err); !strings.Contains(next, "projects create api") || !strings.Contains(next, "--force") {
		t.Fatalf("suggested next = %q, want the create of api with --force", next)
	}
	if got := len(recorder.asked()); got != 1 {
		t.Fatalf("create ran %d jobs, want project-setup once", got)
	}
	if _, statErr := os.Stat(filepath.Join(root, "api", "putnami.json")); statErr != nil {
		t.Fatalf("the failed setup removed the rendered project: %v", statErr)
	}
}

// A create whose template's extensions declare no project-setup needs none.
func TestCreateWithoutAProjectSetupJobSucceeds(t *testing.T) {
	t.Parallel()
	root := starterWorkspace(t)
	if err := createInitProjectFiles(context.Background(), root, wsproto.Load(root),
		[]string{"api", "--template", "starter"}, LifecycleEnv{Out: &bytes.Buffer{}, RunJob: noProjectJob}); err != nil {
		t.Fatalf("create: %v", err)
	}
}

// setupToolchainRelease is the release of the toolchain acme that
// setupToolchainWorkspace pins.
const setupToolchainRelease = "1.2.3"

// setupToolchainWorkspace writes a workspace whose path extension @acme/app
// declares project-setup and a runtime that runs each task with the toolchain
// compiler, locked as acme and found at tools/acme in the workspace, and the
// project api that lists @acme/app. When pinned, the lock pins acme and
// tools/acme runs that release; otherwise the lock pins nothing and the
// workspace holds no tool.
func setupToolchainWorkspace(t *testing.T, pinned bool) string {
	t.Helper()
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"setup-ws","includes":["ext","api"]}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@acme/app"}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.extension.json"), `{
  "name": "@acme/app",
  "version": "0.1.0",
  "cliContract": 4,
  "runtime": {
    "executable": "bin/runtime",
    "toolchains": {
      "compiler": {
        "lock": "acme",
        "candidates": [{"from": "environment", "environment": "PUTNAMI_WORKSPACE_ROOT", "path": "tools/acme"}],
        "probe": {"args": ["--version"], "expect": "{version}"}
      }
    },
    "runToolchains": ["compiler"]
  },
  "commands": {
    "project-setup": {
      "description": "Set a created project up.",
      "visibility": "internal",
      "run": [{ "id": "project-setup", "task": "project-setup-exec" }]
    }
  },
  "tasks": { "project-setup-exec": { "kind": "command", "command": "{extensionRuntime}", "cache": false } }
}`)
	writeRawFile(t, filepath.Join(root, "api", "putnami.json"), `{"name":"api","extensions":["@acme/app"]}`)
	// pinSetupToolchain writes a toolchain pin without archives, which lock
	// format version 4 records.
	lock := lockfile.NewLockFile()
	lock.Version = lockfile.FormatVersionV4
	if pinned {
		pinSetupToolchain(lock, "acme")
		fixtureproc.Write(t, filepath.Join(root, "tools", "acme"), fixtureproc.Program{Stdout: setupToolchainRelease + "\n"})
	}
	if err := lockfile.WriteLockFile(root, lock); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// pinSetupToolchain pins name in lock at setupToolchainRelease, with an
// integrity for this host.
func pinSetupToolchain(lock *lockfile.LockFile, name string) {
	lock.SetToolchain(name, lockfile.LockEntry{
		Version:     setupToolchainRelease,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): "integrity-a"},
	})
}

// toolchainPinning is the toolchain pins phase of a create whose setup
// toolchain does not resolve: it pins the toolchain named lock, as an
// extension answers one from the file the prepare stage wrote, or nothing when
// lock is empty, and counts its runs. A run asked as an implicit install fails
// the test: create is explicit.
type toolchainPinning struct {
	t    *testing.T
	lock string
	runs atomic.Int32
}

func (p *toolchainPinning) pin(_ context.Context, root string, opts toolchainpins.Options) (toolchainpins.Result, error) {
	p.runs.Add(1)
	if opts.Implicit {
		p.t.Errorf("create ran the toolchain pins phase as an implicit install")
	}
	if p.lock == "" {
		return toolchainpins.Result{}, nil
	}
	lock, err := lockfile.ReadLockFile(root)
	if err != nil {
		return toolchainpins.Result{}, err
	}
	pinSetupToolchain(lock, p.lock)
	if err := lockfile.WriteLockFile(root, lock); err != nil {
		return toolchainpins.Result{}, err
	}
	return toolchainpins.Result{Toolchains: lock.Toolchains, Added: []string{p.lock}, Written: true}, nil
}

// jobsAsked names each job request the recorder received with its params.
func jobsAsked(t *testing.T, recorder *jobRecorder) []string {
	t.Helper()
	var asked []string
	for _, req := range recorder.asked() {
		asked = append(asked, req.Job+" "+paramsJSON(t, req))
	}
	return asked
}

// When every toolchain project-setup requires resolves as it does for the
// job, create runs the job once, with no stage, and pins and installs nothing.
func TestCreateRunsTheSetupOnceWhenItsToolchainsResolve(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "project-setup-runs-in-the-project-extension",
		"create-runs-the-setup-once-when-its-toolchains-resolve")
	root := setupToolchainWorkspace(t, true)
	recorder := &jobRecorder{outcome: WorkspaceJobOK}
	pinning := &toolchainPinning{t: t, lock: "acme"}
	var out bytes.Buffer
	installErr, err := setUpProject(context.Background(), root, "api",
		LifecycleEnv{Out: &out, RunJob: recorder.run, pinToolchains: pinning.pin})
	if err != nil || installErr != nil {
		t.Fatalf("setup = %v, install %v\n%s", err, installErr, out.String())
	}
	if got, want := jobsAsked(t, recorder), []string{`project-setup {"path":"api"}`}; !slices.Equal(got, want) {
		t.Fatalf("jobs = %v, want %v", got, want)
	}
	if runs := pinning.runs.Load(); runs != 0 {
		t.Fatalf("the toolchain pins phase ran %d times, want none", runs)
	}
}

// When a toolchain project-setup requires does not resolve, create runs the
// job's prepare stage, then the toolchain pins phase. It runs the workspace installers
// only when that phase pinned a toolchain the extension's runtime locks, and
// then runs the job once.
func TestCreatePreparesPinsAndInstallsOnlyAToolchainTheExtensionLocks(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "project-setup-runs-in-the-project-extension",
		"create-prepares-pins-and-installs-only-a-toolchain-the-extension-locks")
	const (
		prepare = `project-setup {"path":"api","stage":"prepare"}`
		install = "workspace-install null"
		setup   = `project-setup {"path":"api"}`
	)
	cases := []struct {
		name, pins string
		want       []string
	}{
		{"a pin of the toolchain the extension locks", "acme", []string{prepare, install, setup}},
		{"a pin of a toolchain the extension does not lock", "other", []string{prepare, setup}},
		{"no pin", "", []string{prepare, setup}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := setupToolchainWorkspace(t, false)
			recorder := &jobRecorder{outcome: WorkspaceJobOK}
			pinning := &toolchainPinning{t: t, lock: tc.pins}
			var out bytes.Buffer
			installErr, err := setUpProject(context.Background(), root, "api",
				LifecycleEnv{Out: &out, RunJob: recorder.run, pinToolchains: pinning.pin})
			if err != nil || installErr != nil {
				t.Fatalf("setup = %v, install %v\n%s", err, installErr, out.String())
			}
			if got := jobsAsked(t, recorder); !slices.Equal(got, tc.want) {
				t.Fatalf("jobs = %v, want %v", got, tc.want)
			}
			if runs := pinning.runs.Load(); runs != 1 {
				t.Fatalf("the toolchain pins phase ran %d times, want once", runs)
			}
		})
	}
}

// editorWorkspace writes a workspace whose path extension @acme/go declares
// the deps-edit job, with the project api that lists it and the project web
// that lists no extension.
func editorWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"edit-ws","includes":["ext","api","web"]}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@acme/go"}`)
	writeRawFile(t, filepath.Join(root, "ext", "putnami.extension.json"), `{
  "name": "@acme/go",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "deps-edit": {
      "description": "Edit a project's requirements.",
      "visibility": "internal",
      "run": [{ "id": "deps-edit", "task": "deps-edit-exec" }]
    }
  },
  "tasks": { "deps-edit-exec": { "kind": "command", "command": "echo", "cache": false } }
}`)
	writeRawFile(t, filepath.Join(root, "api", "putnami.json"), `{"name":"api","extensions":["@acme/go"]}`)
	writeRawFile(t, filepath.Join(root, "web", "putnami.json"), `{"name":"web"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// deps add and deps remove run the deps-edit job of the project's extension
// for that project only, with the action and one edit that names the project,
// its path and the modules. A failed job fails the command.
func TestDepsAddAndRemoveRunTheDepsEditJobOfTheProject(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "deps-edit-runs-in-the-project-extension",
		"deps-add-and-remove-run-the-deps-edit-job-of-the-project")
	root := editorWorkspace(t)
	apiID := projectIDAt(t, root, "api")
	recorder := &jobRecorder{outcome: WorkspaceJobOK}
	env := LifecycleEnv{Out: &bytes.Buffer{}, RunJob: recorder.run}
	if err := DepsAdd(context.Background(), root, []string{"example.com/x@v1.2.3"}, "", false, env); err != nil {
		t.Fatalf("deps add: %v", err)
	}
	if err := DepsRemove(context.Background(), root, []string{"example.com/y"}, "api", false, env); err != nil {
		t.Fatalf("deps remove: %v", err)
	}
	asked := recorder.asked()
	if len(asked) != 2 {
		t.Fatalf("deps ran %d jobs, want one per command: %+v", len(asked), asked)
	}
	for i, want := range []struct {
		action, module string
	}{{"add", "example.com/x@v1.2.3"}, {"remove", "example.com/y"}} {
		params := `{"action":"` + want.action + `","edits":[{"modules":["` + want.module + `"],"path":"api","project":"api"}]}`
		if req := asked[i]; req.Job != depsEditJob || req.Projects != apiID || paramsJSON(t, req) != params {
			t.Errorf("deps %s ran %s for %q with %s, want %s for %s with %s",
				want.action, req.Job, req.Projects, paramsJSON(t, req), depsEditJob, apiID, params)
		}
	}

	failing := LifecycleEnv{Out: &bytes.Buffer{}, RunJob: (&jobRecorder{outcome: WorkspaceJobFailed}).run}
	if err := DepsAdd(context.Background(), root, []string{"example.com/x"}, "api", false, failing); err == nil ||
		!strings.Contains(err.Error(), "the deps-edit job of @acme/go failed") {
		t.Fatalf("deps add with a failed job = %v, want the failure of the @acme/go job", err)
	}
	if err := DepsAdd(context.Background(), root, []string{"example.com/x"}, "web", false, env); err == nil ||
		!strings.Contains(err.Error(), "cannot edit web") {
		t.Fatalf("deps add for web = %v, want web refused", err)
	}
}

// Under --dry-run, deps add and deps remove hand the deps-edit job the param
// dry-run, true, beside the action and the edit, so the job writes no file.
func TestDepsAddAndRemoveForwardDryRunToTheDepsEditJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "deps-edit-runs-in-the-project-extension",
		"deps-add-and-remove-forward-dry-run-to-the-deps-edit-job")
	root := editorWorkspace(t)
	recorder := &jobRecorder{outcome: WorkspaceJobOK}
	env := LifecycleEnv{Out: &bytes.Buffer{}, RunJob: recorder.run}
	if err := DepsAdd(context.Background(), root, []string{"example.com/x@v1.2.3"}, "api", true, env); err != nil {
		t.Fatalf("deps add --dry-run: %v", err)
	}
	if err := DepsRemove(context.Background(), root, []string{"example.com/y"}, "api", true, env); err != nil {
		t.Fatalf("deps remove --dry-run: %v", err)
	}
	asked := recorder.asked()
	if len(asked) != 2 {
		t.Fatalf("deps --dry-run ran %d jobs, want one per command: %+v", len(asked), asked)
	}
	for i, want := range []struct {
		action, module string
	}{{"add", "example.com/x@v1.2.3"}, {"remove", "example.com/y"}} {
		params := `{"action":"` + want.action + `","dry-run":true,"edits":[{"modules":["` + want.module + `"],"path":"api","project":"api"}]}`
		if got := paramsJSON(t, asked[i]); got != params {
			t.Errorf("deps %s --dry-run params = %s, want %s", want.action, got, params)
		}
	}
}

// deps add refuses a spec at the version none with a usage error before any
// job runs, and suggests the deps remove command for that module with the
// selector it was given.
func TestDepsAddRefusesTheVersionNoneBeforeAnyJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "deps-add-refuses-the-version-none",
		"deps-add-refuses-the-version-none-before-any-job")
	root := editorWorkspace(t)
	for selector, want := range map[string]string{
		"api": "putnami deps remove example.com/lib --projects api",
		"":    "putnami deps remove example.com/lib",
	} {
		recorder := &jobRecorder{outcome: WorkspaceJobOK}
		err := DepsAdd(context.Background(), root, []string{"example.com/lib@none", "example.com/other@v1.0.0"}, selector, false,
			LifecycleEnv{Out: &bytes.Buffer{}, RunJob: recorder.run})
		if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
			t.Fatalf("deps add example.com/lib@none = %v (exit %d), want a usage error", err, code)
		}
		if !strings.Contains(err.Error(), "deps add does not drop a module: example.com/lib names the version none") {
			t.Errorf("error = %v, want it to say deps add does not drop example.com/lib", err)
		}
		if got := protocolcli.SuggestedNext(err); got != want {
			t.Errorf("next command = %q, want %q", got, want)
		}
		if asked := recorder.asked(); len(asked) != 0 {
			t.Errorf("deps add ran %d jobs before it refused the spec", len(asked))
		}
	}
}

// The version of a spec follows its last "@" that is not its first character:
// @scope/pkg@none is refused as @scope/pkg, and the scoped name @scope/pkg,
// which has no version, is not refused.
func TestDepsAddSplitsAScopedSpecAtItsLastAt(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "deps-add-refuses-the-version-none",
		"deps-add-splits-a-scoped-spec-at-its-last-at")
	err := refuseDropOnAdd([]string{"@scope/pkg@none", "@scope/other", "@scope/third@1.0.0", "plain@none"}, "api")
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Fatalf("refuseDropOnAdd = %v (exit %d), want a usage error", err, code)
	}
	if !strings.Contains(err.Error(), "deps add does not drop a module: @scope/pkg, plain names the version none") {
		t.Errorf("error = %v, want it to name @scope/pkg and plain only", err)
	}
	if got, want := protocolcli.SuggestedNext(err), "putnami deps remove @scope/pkg plain --projects api"; got != want {
		t.Errorf("next command = %q, want %q", got, want)
	}
	if err := refuseDropOnAdd([]string{"@scope/pkg", "@scope/pkg@latest", "example.com/lib@v1.0.0"}, ""); err != nil {
		t.Errorf("refuseDropOnAdd refused a spec that names no version none: %v", err)
	}
}

// manifestPruneWorkspace is a project tool whose putnami.json declares
// acme/core and whose language manifest declares acme/lib, and no import backs
// either.
func manifestPruneWorkspace(t *testing.T) (root string, ws *workspace.Workspace) {
	t.Helper()
	root = t.TempDir()
	writeRawFile(t, filepath.Join(root, "tools", "cli", "putnami.json"),
		"{\n  \"name\": \"acme/tool\",\n  \"dependencies\": [\"acme/core\"]\n}\n")
	tool := &workspace.Project{
		ID: "/tools/cli", Name: "acme/tool", Path: "tools/cli",
		Config:       &wsproto.ProjectConfig{Name: "acme/tool", Dependencies: []string{"acme/core"}},
		Dependencies: []string{"acme/core", "acme/lib"},
		DependencySources: map[string]wsproto.DependencySource{
			"acme/lib": wsproto.DependencySourceDeclared,
		},
	}
	return root, workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/libs/core", Name: "acme/core", Path: "libs/core"},
		{ID: "/libs/lib", Name: "acme/lib", Path: "libs/lib"},
		tool,
	})
}

// prune hands a manifest finding to the deps-edit job of the importer's
// extension, with the directory of each target whose requirement it drops,
// after it edits the importer's putnami.json.
func TestDepsPruneHandsAManifestFindingToTheDepsEditJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"prune-hands-a-manifest-finding-to-the-importers-deps-edit-job")
	root, ws := manifestPruneWorkspace(t)
	editors := map[string]string{"/tools/cli": "@acme/go"}
	recorder := &jobRecorder{outcome: WorkspaceJobOK}
	env := LifecycleEnv{RunJob: recorder.run}

	var out bytes.Buffer
	if err := prunePlan(context.Background(), root, ws, editors, "", true, &out, env); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if want := "prune acme/tool → acme/lib [manifest] in the manifest @acme/go edits"; !strings.Contains(out.String(), want) {
		t.Fatalf("dry run = %q, want the line %q", out.String(), want)
	}
	if got := len(recorder.asked()); got != 0 {
		t.Fatalf("the dry run ran %d jobs", got)
	}

	out.Reset()
	if err := prunePlan(context.Background(), root, ws, editors, "", false, &out, env); err != nil {
		t.Fatalf("prune: %v\n%s", err, out.String())
	}
	asked := recorder.asked()
	const params = `{"action":"prune","edits":[{"modules":["libs/lib"],"path":"tools/cli","project":"acme/tool"}]}`
	if len(asked) != 1 || asked[0].Job != depsEditJob || asked[0].Projects != "/tools/cli" || paramsJSON(t, asked[0]) != params {
		t.Fatalf("prune ran %+v, want one %s for /tools/cli with %s", asked, depsEditJob, params)
	}
	if got := readToolConfig(t, root); strings.Contains(got, "acme/core") {
		t.Fatalf("putnami.json still declares acme/core:\n%s", got)
	}
}

// A failed deps-edit job puts back the putnami.json prune edited before it, so
// a rerun reads the provider view this run read.
func TestDepsPrunePutsPutnamiJSONBackWhenTheDepsEditJobFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"a-failed-deps-edit-puts-putnami-json-back")
	root, ws := manifestPruneWorkspace(t)
	before := readToolConfig(t, root)
	env := LifecycleEnv{RunJob: (&jobRecorder{outcome: WorkspaceJobFailed}).run}
	var out bytes.Buffer
	err := prunePlan(context.Background(), root, ws, map[string]string{"/tools/cli": "@acme/go"}, "", false, &out, env)
	if err == nil || !strings.Contains(err.Error(), "the deps-edit job of @acme/go failed") {
		t.Fatalf("prune = %v, want the failure of the @acme/go job", err)
	}
	if after := readToolConfig(t, root); after != before {
		t.Fatalf("putnami.json after the failed job =\n%s\nwant it as it was:\n%s", after, before)
	}
}
