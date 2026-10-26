package engine

import (
	"context"
	"io"
	"reflect"
	"sync/atomic"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/watch"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Watch is a replan policy over Engine.Run. Routing a surface
// through the engine grants it the WHOLE lifecycle unless the adapter withholds
// it (A4 and A5a both learned this), so these tests pin what one watch iteration
// is and is not allowed to do.

func watchTestProjects() []*workspace.Project {
	return []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
}

func outerWatchRequest() *Request {
	return &Request{
		WorkspaceRoot: "/ws",
		Commands:      []string{"build"},
		CommandParams: map[string]any{"port": 3000},
		Global: GlobalFlags{
			Watch:      true,
			All:        true,
			Projects:   "*",
			CacheTrust: string(store.CacheTrustAny),
			EnvProfile: "production",
			FilterTag:  "web",
			ExcludeTag: "slow",
			Exclude:    "docs",
		},
	}
}

// A hosted run never enters the watch loop, even when the CLI's own discovery
// found no serve-workload command and let the run start: the engine refuses it
// as a usage error from the extensions it planned with. It sets the process's
// run credential (runcredential.SetForTest), so it does not run in parallel.
func TestAHostedRunNeverEntersTheWatchLoop(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-hosted-run-refuses-a-serve-workload-command")
	defer runcredential.SetForTest("prc_engine_watch")()
	result := New().runWatch(context.Background(), outerWatchRequest(), nil, watchTestProjects(), nil, nil, true)
	if result.ExitCode != ExitUsage {
		t.Fatalf("a hosted watch exited %d, want %d", result.ExitCode, ExitUsage)
	}
}

// The successful-run marker is the sharpest thing an iteration could inherit: it
// is keyed (branch, commands, params) → HEAD with no provenance, so a marker
// written after rebuilding ONE project would let the next --impacted run treat
// every commit up to HEAD as already built. The outer run here is auto-selected,
// which is exactly the state that DOES record a marker.
func TestWatchIteration_NeverRecordsASuccessfulRunMarker(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()
	outer.Global.AutoSelected = true
	outer.Global.All = true
	outer.Global.FilterTag, outer.Global.ExcludeTag, outer.Global.Exclude = "", "", ""

	if !shouldRecordSuccessfulBuild(outer) {
		t.Fatalf("fixture is wrong: the OUTER run must be marker-eligible for this test to mean anything")
	}

	iteration := watchIterationRequest(outer, nil, watchTestProjects(), false)
	if shouldRecordSuccessfulBuild(&iteration) {
		t.Fatalf("a watch iteration recorded a successful-run marker: flags = %+v", iteration.Global)
	}
}

// Watch reports session:start exactly once, from the outer terminal run's
// initial plan (ADR 0001 §4). A per-iteration observer turns one `putnami serve`
// into a session report per file save.
func TestWatchIteration_CarriesNoObserverNoHooksNoValidation(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()
	outer.Observer = &recordingObserver{}
	outer.Hooks = &wsproto.HooksConfig{}
	outer.ValidateCommandFlags = func(*extension.DiscoveryResult, GlobalFlags) error { return nil }

	iteration := watchIterationRequest(outer, nil, watchTestProjects(), false)

	if iteration.Observer != nil {
		t.Error("watch iteration carries a telemetry observer; ADR 0001 §4 requires nil")
	}
	if iteration.Hooks != nil {
		t.Error("watch iteration carries lifecycle hooks; the OUTER run brackets the whole session")
	}
	if iteration.ValidateCommandFlags != nil {
		t.Error("watch iteration re-validates the command line; the outer run did that once")
	}
	if iteration.WorkspaceLifecycle {
		t.Error("watch iteration claims to be a workspace lifecycle run")
	}
}

func TestWatchIterationFlags_SelectionAndCachePolicy(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()

	g := watchIterationFlags(outer.Global, watchTestProjects(), false)

	if g.Watch {
		t.Error("iteration re-enters watch mode; the loop IS the replan policy")
	}
	if g.Projects != "/packages/app,/packages/lib" {
		t.Errorf("iteration projects = %q, want the classified project IDs", g.Projects)
	}
	if g.All || g.Impacted || g.AutoSelected {
		t.Errorf("iteration kept a bare/auto selection: all=%v impacted=%v auto=%v", g.All, g.Impacted, g.AutoSelected)
	}
	if g.FilterTag != "" || g.ExcludeTag != "" || g.Exclude != "" {
		t.Errorf("iteration kept selection filters (%q/%q/%q); the loop already resolved the set",
			g.FilterTag, g.ExcludeTag, g.Exclude)
	}
	if g.CacheTrust != string(store.CacheTrustNone) {
		t.Errorf("iteration cache trust = %q, want %q — watch never negotiated a remote cache",
			g.CacheTrust, store.CacheTrustNone)
	}
	if g.EnvProfile != "" {
		t.Errorf("iteration env profile = %q, want empty: the production preflight is an entry "+
			"gate the outer run already passed, not a per-save doctor scan", g.EnvProfile)
	}
	if !g.NoCache {
		t.Error("a non-serve watch iteration must run with the cache off, as it did before A5b")
	}
	if g.NoCacheExplicit != outer.Global.NoCacheExplicit {
		t.Errorf("iteration NoCacheExplicit = %v, want the outer run's %v: the forced cache-off "+
			"is the host's choice and must not reach extensions as a typed --no-cache",
			g.NoCacheExplicit, outer.Global.NoCacheExplicit)
	}
}

// Serve is the one watch mode that keeps the build cache, so a restart can skip
// generate steps it already produced.
func TestWatchIterationFlags_ServeModeKeepsTheLocalCache(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()
	outer.Global.NoCache = false

	g := watchIterationFlags(outer.Global, watchTestProjects(), true)

	if g.NoCache {
		t.Error("serve iteration disabled the local cache; restarts then re-run every generate step")
	}
	if g.CacheTrust != string(store.CacheTrustNone) {
		t.Errorf("serve iteration cache trust = %q, want %q (local cache on, remote off)",
			g.CacheTrust, store.CacheTrustNone)
	}
}

// --no-cache still wins: the loop only ever tightens the cache policy.
func TestWatchIterationFlags_ServeModeHonoursNoCache(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()
	outer.Global.NoCache = true
	outer.Global.NoCacheExplicit = true

	g := watchIterationFlags(outer.Global, watchTestProjects(), true)
	if !g.NoCache {
		t.Error("serve iteration re-enabled the cache the user turned off")
	}
	if !g.NoCacheExplicit {
		t.Error("serve iteration dropped the user's typed --no-cache; extensions would no longer see it")
	}
}

func TestWatchIterationRequest_ForwardsTheRunsIdentity(t *testing.T) {
	t.Parallel()
	outer := outerWatchRequest()
	outer.ExecutesUnderDryRun = true
	outer.VersionSnapshot = &git.VersionInfo{Full: "1.2.3"}
	outer.Global.ServeEnv = "-"
	outer.ServeProcessEnv = []string{"PORT=0"}
	outer.latestWarnings = &atomic.Pointer[workspace.WarningReport]{}
	planExtensions := []*extension.ExtensionDescription{{Name: "@putnami/one"}}

	iteration := watchIterationRequest(outer, planExtensions, watchTestProjects(), false)

	if iteration.WorkspaceRoot != outer.WorkspaceRoot || !reflect.DeepEqual(iteration.Commands, outer.Commands) {
		t.Errorf("iteration = %s %v, want the outer run's root and commands", iteration.WorkspaceRoot, iteration.Commands)
	}
	// A param value's Go TYPE is part of every cache key (store.hashParams), so the
	// map is forwarded, never rebuilt.
	if !reflect.DeepEqual(iteration.CommandParams, outer.CommandParams) {
		t.Errorf("iteration params = %#v, want %#v", iteration.CommandParams, outer.CommandParams)
	}
	// An extension alias names ONE extension's flat command; replanning against the
	// full discovered set would run every extension's job of the same name.
	if !reflect.DeepEqual(iteration.PlanExtensions, planExtensions) {
		t.Errorf("iteration plan extensions = %v, want the outer run's narrowed set", iteration.PlanExtensions)
	}
	// Recapturing would fold iteration N-1's generated output into a false
	// "-<dirtyhash>" stamp, and the stamp is part of every cache key.
	if iteration.VersionSnapshot != outer.VersionSnapshot {
		t.Error("iteration recaptures the version stamp instead of forwarding the run's")
	}
	// The session's renderer reads the load of the latest iteration through it.
	if iteration.latestWarnings != outer.latestWarnings {
		t.Error("iteration does not share the session's latest-load cell; its envelope would keep the first load's warnings")
	}
	if !iteration.ExecutesUnderDryRun {
		t.Error("iteration dropped ExecutesUnderDryRun; an alias's --dry-run iterations would preview forever")
	}
	// A restarted serve step is a new process: the supervisor's environment
	// must reach it as it reached the first one.
	if !reflect.DeepEqual(iteration.ServeProcessEnv, outer.ServeProcessEnv) || iteration.Global.ServeEnv != "-" {
		t.Errorf("iteration serve env = %v (flag %q), want the outer run's %v", iteration.ServeProcessEnv, iteration.Global.ServeEnv, outer.ServeProcessEnv)
	}
	if iteration.Stdout != io.Discard {
		t.Error("iteration engine notices are not discarded; 'No jobs matched' would print on every save")
	}
}

// The loop classifies changed files against the whole workspace, so it routinely
// selects a project that declares no job for the run's commands. Before A5b that
// was a 0-exit no-op; the engine's verdict for an empty plan is ExitUsage.
func TestWatchIterationResult_ExitCodeMapping(t *testing.T) {
	t.Parallel()
	failed := &jobs.SessionResult{}
	aborted := &jobs.SessionResult{Aborted: true}

	tests := []struct {
		name    string
		result  SessionResult
		want    watch.IterationResult
		comment string
	}{
		{
			name:   "nothing planned is a no-op, not a usage error",
			result: SessionResult{ExitCode: ExitUsage},
			want:   watch.IterationResult{ExitCode: protocolcli.ExitSuccess},
		},
		{
			name:   "a plan error keeps its failure code",
			result: SessionResult{ExitCode: ExitError},
			want:   watch.IterationResult{ExitCode: protocolcli.ExitFailure},
		},
		{
			name:   "a failed run keeps its failure code",
			result: SessionResult{ExitCode: ExitError, Session: failed},
			want:   watch.IterationResult{ExitCode: protocolcli.ExitFailure},
		},
		{
			name:   "a successful run",
			result: SessionResult{ExitCode: ExitSuccess, Session: failed},
			want:   watch.IterationResult{ExitCode: protocolcli.ExitSuccess},
		},
		{
			name:   "an aborted run is flagged, not failed",
			result: SessionResult{ExitCode: ExitSignalReceived, Session: aborted},
			want:   watch.IterationResult{ExitCode: protocolcli.ExitSignal, Aborted: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := watchIterationResult(tc.result)
			if got != tc.want {
				t.Fatalf("watchIterationResult(%+v) = %+v, want %+v", tc.result, got, tc.want)
			}
			if got.ExitCode == protocolcli.ExitUsage {
				t.Fatalf("an iteration must never report ExitUsage (%d): an earlier failed "+
					"iteration returned a raw 2, which collides with 'you used the CLI wrong'",
					protocolcli.ExitUsage)
			}
		})
	}
}

func TestWorkspaceScopedInputPatterns(t *testing.T) {
	t.Parallel()
	extensions := []*extension.ExtensionDescription{
		nil,
		{
			Name: "@putnami/typescript",
			Tasks: map[string]extension.TaskDefinition{
				"build-transpile": {Inputs: map[string]extension.TaskInputPort{
					"sources":   {From: "project", Files: []string{"**/*.ts"}},
					"workspace": {From: "workspace", Files: []string{"tsconfig.base.json", "bun.lock"}},
				}},
				"test-exec": {Inputs: map[string]extension.TaskInputPort{
					"lock": {From: "workspace", Files: []string{"bun.lock", ""}},
				}},
			},
		},
	}

	got := workspaceScopedInputPatterns(extensions)

	want := []string{"bun.lock", "tsconfig.base.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace-scoped patterns = %v, want %v (project-scoped inputs excluded, "+
			"deduplicated, sorted)", got, want)
	}
}

// A provider's declared metadata inputs and markers widen the same seam. They
// must reach a watch iteration, because the iteration is
// where the workspace snapshot is re-validated and the one owning provider is
// re-probed; a metadata input no iteration ever noticed would leave the
// workspace resolved from a stale index until the next command outside the loop.
func TestWorkspaceScopedInputPatterns_IncludesWorkspaceAdapterInputs(t *testing.T) {
	t.Parallel()
	extensions := []*extension.ExtensionDescription{
		{
			Name: "@putnami/typescript",
			Tasks: map[string]extension.TaskDefinition{
				"test-exec": {Inputs: map[string]extension.TaskInputPort{
					"lock": {From: "workspace", Files: []string{"bun.lock"}},
				}},
			},
			Workspace: &extproto.WorkspaceAdapter{
				// package.json appears in BOTH halves; the union must dedupe.
				Markers: []string{"package.json"},
				Inputs:  []string{"package.json", "bun.lock", "tsconfig.json"},
			},
		},
		{Name: "@putnami/go", Workspace: &extproto.WorkspaceAdapter{
			Markers: []string{"go.mod"},
			Inputs:  []string{"go.mod", "**/*.go", "go.work"},
		}},
	}

	got := workspaceScopedInputPatterns(extensions)

	want := []string{"bun.lock", "go.mod", "go.work", "package.json", "tsconfig.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns = %v, want %v (root-shaped adapter inputs and markers unioned, "+
			"candidate-relative **/*.go omitted)", got, want)
	}
}

// A run streams its workload when one of its commands declares a workload in
// the loaded extensions, whatever its name: the serve workload is promoted to
// watch, the once workload streams once. A serve or run command that declares
// none streams nothing and is not promoted.
func TestWorkloadModes_FollowTheDeclaredWorkloadNotTheName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-serve-workload-command-of-any-name-streams-until-interrupted")
	command := func(name, workload string) *extension.JobDefinition {
		return &extension.JobDefinition{Name: name, Kind: "command", Traits: extension.CommandTraits{Workload: workload}}
	}
	extensions := []*extension.ExtensionDescription{{Name: "@acme/lang", Jobs: map[string]*extension.JobDefinition{
		"daemon":  command("daemon", extension.WorkloadServe),
		"migrate": command("migrate", extension.WorkloadOnce),
		"serve":   command("serve", ""),
		"run":     command("run", ""),
	}}}
	for _, tc := range []struct {
		commands    []string
		serve, once bool
	}{
		{commands: []string{"daemon"}, serve: true},
		{commands: []string{"build", "daemon"}, serve: true},
		{commands: []string{"migrate"}, once: true},
		{commands: []string{"serve"}},
		{commands: []string{"run"}},
	} {
		serve, once := workloadModes(extensions, tc.commands)
		if serve != tc.serve || once != tc.once {
			t.Errorf("workloadModes(%v) = (%v, %v), want (%v, %v)", tc.commands, serve, once, tc.serve, tc.once)
		}
		if got := forcesServeWatch(&Request{Commands: tc.commands}, serve); got != tc.serve {
			t.Errorf("%v promoted to watch: %v, want %v", tc.commands, got, tc.serve)
		}
	}
	if serve, once := workloadModes(nil, []string{"serve", "run"}); serve || once {
		t.Errorf("without a loaded extension, serve and run stream: (%v, %v)", serve, once)
	}
}

// Serve watches by default, and --no-watch, which reaches the run as the serve
// flag `watch: false`, keeps it out of the restart loop. An iteration of an
// outer watch loop is never promoted again.
func TestForcesServeWatch_NoWatchKeepsServeOutOfTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		watch     any // the run's `watch` param; nil leaves it out
		serve     bool
		iteration bool
		want      bool
	}{
		{name: "serve by default", serve: true, want: true},
		{name: "serve --watch=true", watch: true, serve: true, want: true},
		{name: "serve --no-watch", watch: false, serve: true, want: false},
		{name: "a watch iteration", serve: true, iteration: true, want: false},
		{name: "not serve", serve: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{}
			if tc.watch != nil {
				params["watch"] = tc.watch
			}
			req := &Request{CommandParams: params, watchIteration: tc.iteration}
			if got := forcesServeWatch(req, tc.serve); got != tc.want {
				t.Errorf("forcesServeWatch = %v, want %v", got, tc.want)
			}
		})
	}
}
