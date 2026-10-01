package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Request.WorkspaceLifecycle is the seam that keeps
// `putnami install` from inheriting the whole build lifecycle the moment its
// workspace-install pass started routing through Engine.Run. Each test below
// pins one of the four documented differences; they fail BEFORE a stage that
// forgets one ships, not after a user notices their install wrote a session.

// scheduledJobFor builds a plan entry attributed to extension name over project.
func scheduledJobFor(extName string, proj *workspace.Project) *jobs.ScheduledJob {
	ext := &extension.ExtensionDescription{Name: extName, Path: "/ext/" + extName}
	return &jobs.ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "workspace-install",
			ExtensionName: extName,
			Command:       "/bin/true",
		},
	}
}

// A workspace-level job runs once per PROVIDER, not once per project: a
// 20-project workspace must run one `bun install`, not twenty. Before A5a this
// was a private filter in internal/commands/deps.go.
func TestDedupePlanByExtension_KeepsOneJobPerProvider(t *testing.T) {
	t.Parallel()
	a := &workspace.Project{ID: "/a", Name: "a", Path: "a"}
	b := &workspace.Project{ID: "/b", Name: "b", Path: "b"}
	c := &workspace.Project{ID: "/c", Name: "c", Path: "c"}
	planned := []*jobs.ScheduledJob{
		scheduledJobFor("@putnami/typescript", a),
		scheduledJobFor("@putnami/typescript", b),
		scheduledJobFor("@putnami/go", c),
		scheduledJobFor("@putnami/typescript", c),
	}

	got := dedupePlanByExtension(planned)

	if len(got) != 2 {
		t.Fatalf("deduped plan = %d jobs, want 2 (one per provider)", len(got))
	}
	// The FIRST job of each extension survives, in plan order, so the choice is
	// deterministic run to run.
	if got[0] != planned[0] || got[1] != planned[2] {
		t.Errorf("deduped plan kept the wrong entries: %v", got)
	}
}

// Dropping planned work silently is the one failure mode this filter must never
// have, so a job with no extension is kept rather than deduped away.
func TestDedupePlanByExtension_KeepsUnattributedJobs(t *testing.T) {
	t.Parallel()
	proj := &workspace.Project{ID: "/a", Name: "a", Path: "a"}
	orphan := &jobs.ScheduledJob{Project: proj, JobDef: &extension.JobDefinition{Name: "workspace-install"}}
	planned := []*jobs.ScheduledJob{scheduledJobFor("@putnami/go", proj), orphan}

	if got := dedupePlanByExtension(planned); len(got) != 2 {
		t.Fatalf("deduped plan = %d jobs, want both kept", len(got))
	}
}

// The reused-backing-array trick the pre-A5a filter used (planned[:0]) is only
// safe while nobody else holds the plan; the engine hands it back on
// SessionResult.Plan, so the filter must not overwrite its input.
func TestDedupePlanByExtension_DoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	proj := &workspace.Project{ID: "/a", Name: "a", Path: "a"}
	first := scheduledJobFor("@putnami/typescript", proj)
	second := scheduledJobFor("@putnami/typescript", proj)
	third := scheduledJobFor("@putnami/go", proj)
	planned := []*jobs.ScheduledJob{first, second, third}

	_ = dedupePlanByExtension(planned)

	if planned[0] != first || planned[1] != second || planned[2] != third {
		t.Error("dedupePlanByExtension overwrote the plan it was given")
	}
}

func TestRunSynchronizesExtensionRuntimeBeforePlanning(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"runtime-sync","includes":["app","extension"]}`)
	write("app/putnami.json", `{"name":"app"}`)
	write("extension/putnami.json", `{"name":"@putnami/runtime-sync"}`)
	write("extension/putnami.extension.json", `{
		"name": "@putnami/runtime-sync",
		"version": "1.0.0",
		"cliContract": 4,
		"runtime": {"executable": "compiled/missing"},
		"commands": {
			"build": {"run": [{"id": "build", "task": "build-exec"}]}
		},
		"tasks": {
			"build-exec": {
				"kind": "command",
				"command": "{extensionRuntime}"
			}
		}
	}`)
	workspace.InvalidateLoadCache(root)

	var result SessionResult
	stderr := captureStderr(t, func() {
		result, _ = New().Run(context.Background(), Request{
			WorkspaceRoot: root,
			Config:        &wsproto.Config{},
			Commands:      []string{"build"},
			Global: GlobalFlags{
				Plan:     true,
				Projects: "*",
			},
			Stdout: io.Discard,
		}, nil)
	})
	if result.ExitCode != ExitError {
		t.Fatalf("runtime synchronization exit code = %d, want %d", result.ExitCode, ExitError)
	}
	if result.Plan != nil {
		t.Fatal("engine built a plan before synchronizing extension runtimes")
	}
	if !strings.Contains(stderr, "runtime.executable_missing") {
		t.Fatalf("runtime synchronization lost typed failure: %s", stderr)
	}
}

func TestRunLifecycleBootstrapSkipsUnusedExtensionRuntime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"runtime-bootstrap","includes":["app","extension"]}`, 0o644)
	write("app/putnami.json", `{"name":"app"}`, 0o644)
	write("extension/putnami.json", `{"name":"@putnami/runtime-bootstrap","extensions":["/extension"]}`, 0o644)
	write("extension/bin/workspace-install", "#!/bin/sh\nexit 0\n", 0o755)
	write("extension/cmd/main.go", "package main\n", 0o644)
	write("extension/putnami.extension.json", `{
		"name": "@putnami/runtime-bootstrap",
		"version": "1.0.0",
		"cliContract": 4,
		"runtime": {
			"executable": "compiled/missing",
			"prepare": {
				"command": "{extensionRoot}/bin/missing-prepare",
				"inputs": ["cmd/**"]
			}
		},
		"commands": {
			"workspace-install": {"run": [{"id": "install", "task": "workspace-install-exec"}]},
			"build": {"run": [{"id": "build", "task": "build-exec"}]}
		},
		"tasks": {
			"workspace-install-exec": {
				"kind": "command",
				"command": "{extensionRoot}/bin/workspace-install",
				"cache": false
			},
			"build-exec": {
				"kind": "command",
				"command": "{extensionRuntime}"
			}
		}
	}`, 0o644)
	workspace.InvalidateLoadCache(root)

	result, err := New().Run(context.Background(), Request{
		WorkspaceRoot:      root,
		Config:             &wsproto.Config{},
		Commands:           []string{"workspace-install"},
		WorkspaceLifecycle: true,
		Global: GlobalFlags{
			Plan:     true,
			Projects: "*",
		},
		Stdout: io.Discard,
	}, nil)
	if err != nil {
		t.Fatalf("lifecycle bootstrap: %v", err)
	}
	if result.ExitCode != ExitSuccess {
		t.Fatalf("lifecycle bootstrap exit code = %d, want %d", result.ExitCode, ExitSuccess)
	}
	if len(result.Plan) != 1 {
		t.Fatalf("lifecycle bootstrap plan = %d jobs, want 1", len(result.Plan))
	}
	if result.Plan[0].Extension.RuntimeExecutable != "" {
		t.Fatalf("lifecycle bootstrap prepared runtime %q before its recovery job", result.Plan[0].Extension.RuntimeExecutable)
	}
}

// A lifecycle run records NO session file. `putnami install` is not a build, and
// a recorded install session would silently become what `putnami sessions show`
// reads as the latest run.
func TestExecute_LifecycleRunWritesNoSession(t *testing.T) {
	t.Parallel()
	sessionsDir := func(f *executeFixture) string {
		return filepath.Join(f.wsRoot, ".putnami", "sessions")
	}

	// Control: an ordinary run DOES record one, so the assertion below is a
	// genuine difference and not a fixture that never writes sessions.
	control := newExecuteFixture(t)
	control.req.Global.NoCache = true
	_ = control.engine.execute(context.Background(), control.req, control.ws,
		[]*workspace.Project{control.project},
		&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{control.ext}},
		control.planned, nil, discardEvents{})
	if entries, err := os.ReadDir(sessionsDir(control)); err != nil || len(entries) == 0 {
		t.Fatalf("control run recorded no session (%v, %d entries) — fixture cannot prove the lifecycle case", err, len(entries))
	}

	f := newExecuteFixture(t)
	f.req.Global.NoCache = true
	f.req.WorkspaceLifecycle = true
	code := f.engine.execute(context.Background(), f.req, f.ws,
		[]*workspace.Project{f.project},
		&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}},
		f.planned, nil, discardEvents{}).ExitCode
	if code != ExitSuccess {
		t.Fatalf("lifecycle run exit code = %d, want %d", code, ExitSuccess)
	}

	entries, err := os.ReadDir(sessionsDir(f))
	if err == nil && len(entries) > 0 {
		t.Errorf("lifecycle run wrote %d session entries, want none", len(entries))
	}
}

// A lifecycle run publishes NO successful-run marker. Markers are keyed by
// (branch, commands, params) with no provenance, so one written by `putnami
// install` would let a later --impacted run treat everything up to HEAD as
// already built.
func TestShouldRecordSuccessfulBuild_LifecycleNeverRecords(t *testing.T) {
	t.Parallel()
	req := &Request{Commands: []string{"workspace-install"}}
	// The most permissive marker-recording shape there is: a bare --all run.
	req.Global.All = true

	if !shouldRecordSuccessfulBuild(req) {
		t.Fatal("control: an --all build must record a marker, or this test proves nothing")
	}

	req.WorkspaceLifecycle = true
	if shouldRecordSuccessfulBuild(req) {
		t.Error("a lifecycle run must never publish a successful-run marker")
	}
}

// Two guards a lifecycle run must NOT be subject to, because a lifecycle run is
// what repairs the state they describe: an empty plan is a no-op rather than a
// usage error, and it is never escalated into "run `putnami install`" — which,
// during `putnami install`, is advice with nowhere to go.
func TestBuildPlan_LifecycleEmptyPlanIsASuccessfulNoOp(t *testing.T) {
	wsRoot := lifecycleFixtureWorkspace(t)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{}
	// A declared-but-unloaded registry extension is exactly what trips the
	// missing-extension escalation on the ordinary path.
	cfg.Extensions.List = map[string]string{"@putnami/absent": "1.0.0"}

	newReq := func(lifecycle bool) *Request {
		return &Request{
			WorkspaceRoot:      wsRoot,
			Config:             cfg,
			Commands:           []string{"workspace-install"},
			WorkspaceLifecycle: lifecycle,
			Stdout:             io.Discard,
		}
	}

	_ = captureStderr(t, func() {
		if _, code := buildPlan(newReq(false), ws, ws.Projects, nil, nil); code != ExitError {
			t.Errorf("control: a build over a missing extension must fail (%d), or this test proves nothing", code)
		}
		planned, code := buildPlan(newReq(true), ws, ws.Projects, nil, nil)
		if code != ExitSuccess {
			t.Errorf("lifecycle empty plan exit code = %d, want %d", code, ExitSuccess)
		}
		if len(planned) != 0 {
			t.Errorf("lifecycle empty plan = %d jobs, want 0", len(planned))
		}
	})
}

// The same leniency at the selection stage: `putnami install` over a workspace
// whose projects are all tag-excluded has always exited 0.
func TestSelectProjects_LifecycleEmptySelectionIsASuccessfulNoOp(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "target-selection", "an-empty-selection-is-a-usage-error")
	wsRoot := lifecycleFixtureWorkspace(t)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		t.Fatal(err)
	}

	newReq := func(lifecycle bool) *Request {
		req := &Request{
			WorkspaceRoot:      wsRoot,
			Config:             &wsproto.Config{},
			Commands:           []string{"workspace-install"},
			WorkspaceLifecycle: lifecycle,
			Stdout:             io.Discard,
		}
		req.Global.Projects = "*"
		req.Global.FilterTag = "no-project-has-this-tag"
		return req
	}

	if _, code := selectProjects(newReq(false), ws); code != ExitUsage {
		t.Errorf("control: an ordinary empty selection is a usage error (%d), or this test proves nothing", code)
	}
	if _, code := selectProjects(newReq(true), ws); code != ExitSuccess {
		t.Errorf("lifecycle empty selection exit code = %d, want %d", code, ExitSuccess)
	}
}

// A workspace that disables a job for its BUILDS must still get its
// dependencies installed: honoring disable.jobs here would turn `putnami
// install` into a silent no-op on exactly those workspaces.
func TestBuildPlan_LifecycleIgnoresDisableLists(t *testing.T) {
	wsRoot := lifecycleFixtureWorkspace(t)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Disable: &wsproto.DisableConfig{Jobs: []string{"workspace-install"}}}
	ext := &extension.ExtensionDescription{
		Name: "@putnami/test",
		Path: "/ext/test",
		Jobs: map[string]*extension.JobDefinition{
			"workspace-install": {
				Name:          "workspace-install",
				ExtensionName: "@putnami/test",
				Command:       "/bin/true",
				// Workspace activation is the real shape of a workspace-install job:
				// it applies to every project without each one declaring the
				// extension, which is also why the plan needs deduping.
				Activation: "workspace",
			},
		},
	}

	newReq := func(lifecycle bool) *Request {
		return &Request{
			WorkspaceRoot:      wsRoot,
			Config:             cfg,
			Commands:           []string{"workspace-install"},
			WorkspaceLifecycle: lifecycle,
			Stdout:             io.Discard,
		}
	}

	_ = captureStderr(t, func() {
		if planned, _ := buildPlan(newReq(false), ws, ws.Projects, []*extension.ExtensionDescription{ext}, nil); len(planned) != 0 {
			t.Errorf("control: disable.jobs must drop the job on the build path, got %d jobs", len(planned))
		}
		planned, code := buildPlan(newReq(true), ws, ws.Projects, []*extension.ExtensionDescription{ext}, nil)
		if code != ExitSuccess || len(planned) == 0 {
			t.Errorf("a lifecycle run must plan a disabled job anyway; got %d jobs, code %d", len(planned), code)
		}
	})
}

// lifecycleFixtureWorkspace writes a one-project workspace on disk.
func lifecycleFixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"lifecycle-fixture","includes":["app"]}`)
	write("app/putnami.json", `{"name":"app"}`)
	workspace.InvalidateLoadCache(dir)
	return dir
}

// discardEvents is an EventSink that renders nothing, so an execute-level test
// does not paint the test log.
type discardEvents struct{}

func (discardEvents) Start([]*jobs.ScheduledJob)                             {}
func (discardEvents) JobStart(*jobs.ScheduledJob)                            {}
func (discardEvents) JobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)          {}
func (discardEvents) JobComplete(*jobs.ScheduledJob, *jobs.JobResult)        {}
func (discardEvents) Finish(map[string]*jobs.JobResult, jobs.SessionOutcome) {}

// TestShouldRecordSuccessfulBuild_NarrowedPlanNeverRecords pins the fix for a
// cross-slice defect the whole-epic integration review found: extracting the
// run-marker machinery relied on the terminal path's implicit invariant that
// a run covers every provider of its commands, and planning extension aliases
// against a single extension broke that invariant later. Neither change was
// wrong on its own.
//
// The marker key has no extension dimension, so a marker written by
// `putnami <group> <sub>` claims the whole flat command is built at HEAD. If two
// extensions declare the same flat command, a later bare `putnami <command>`
// reads that marker and silently skips the other extension's jobs.
func TestShouldRecordSuccessfulBuild_NarrowedPlanNeverRecords(t *testing.T) {
	t.Parallel()
	req := &Request{Commands: []string{"deploy"}}
	req.Global.AutoSelected = true

	if !shouldRecordSuccessfulBuild(req) {
		t.Fatal("control: a bare auto-selected run must record a marker, or this test proves nothing")
	}

	req.PlanExtensions = []*extension.ExtensionDescription{{Name: "@putnami/cloud"}}
	if shouldRecordSuccessfulBuild(req) {
		t.Error("a run planned against a narrowed extension set must not publish a marker " +
			"claiming the whole command is built: the key carries no extension dimension")
	}
}

// TestEphemeralSession_KeepsARunOutOfTheLatestRotation pins the other half of the
// same review's findings. `latest` is a measurement surface — `putnami sessions
// gate` defaults to it and --update-baseline --scenario records whatever it
// resolves — so a run that is not the user's build must not rotate it.
func TestEphemeralSession_KeepsARunOutOfTheLatestRotation(t *testing.T) {
	t.Parallel()
	if (&Request{}).EphemeralSession {
		t.Fatal("EphemeralSession must default to false: every ordinary run still owns `latest`")
	}
	if !(&Request{EphemeralSession: true}).EphemeralSession {
		t.Fatal("EphemeralSession must be settable")
	}
}
