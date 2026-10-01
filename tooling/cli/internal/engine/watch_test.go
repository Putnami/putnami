package engine

import (
	"io"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
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
	if !iteration.ExecutesUnderDryRun {
		t.Error("iteration dropped ExecutesUnderDryRun; an alias's --dry-run iterations would preview forever")
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
