package watch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func makeTestWorkspace() *workspace.Workspace {
	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils"},
	}
	ws := workspace.NewWorkspace("/workspace", nil, projects)
	ws.Name = "test-ws"
	return ws
}

// recordingRunner is a fake engine seam: it records the projects each iteration
// was asked to run and returns a scripted verdict.
type recordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	outcome IterationResult
}

func (r *recordingRunner) run(_ context.Context, projects []*workspace.Project, _ jobs.Renderer) IterationResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	r.calls = append(r.calls, names)
	return r.outcome
}

func (r *recordingRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func newIterationSession(cfg SessionConfig) *Session {
	if cfg.Workspace == nil {
		cfg.Workspace = makeTestWorkspace()
	}
	if cfg.Renderer == nil {
		cfg.Renderer = &fakeRenderer{}
	}
	return &Session{
		cfg:       cfg,
		watchRend: NewWatchRenderer(cfg.Renderer, &syncBuffer{}, cfg.ServeMode),
	}
}

// The loop replans over the SHARED change→project calculation
// (workspace.ProjectsForChangedFiles), not a private classifier: a changed file
// selects its project plus every transitive dependent.
func TestSessionAffectedProjects_UsesSharedImpactCalculation(t *testing.T) {
	s := newIterationSession(SessionConfig{})

	got := s.affectedProjects([]string{"packages/lib/src/index.ts"})

	names := make([]string, len(got))
	for i, p := range got {
		names[i] = p.Name
	}
	if len(names) != 2 || names[0] != "app" || names[1] != "lib" {
		t.Fatalf("affected = %v, want [app lib] (lib plus its dependent)", names)
	}
}

// The one rule the deleted classifier had that --impacted does not: a
// workspace-scoped task input (a lockfile, a root tsconfig) belongs to no project
// by path but re-runs every project. It reaches the shared calculation as
// SessionConfig.WorkspaceInputPatterns; dropping it silently stops watch from
// rebuilding on `bun install`.
func TestSessionAffectedProjects_WorkspaceInputPatternsStillAffectEveryProject(t *testing.T) {
	s := newIterationSession(SessionConfig{
		WorkspaceInputPatterns: []string{"bun.lock", "tsconfig.base.json"},
	})

	if got := s.affectedProjects([]string{"bun.lock"}); len(got) != 3 {
		t.Fatalf("lockfile change affected %d projects, want all 3", len(got))
	}

	bare := newIterationSession(SessionConfig{})
	if got := bare.affectedProjects([]string{"bun.lock"}); len(got) != 0 {
		t.Fatalf("without patterns a lockfile affected %d projects, want 0", len(got))
	}
}

// A watch session whose engine seam is unwired would report a clean iteration
// forever while never rebuilding anything.
func TestRunIterationForProjects_NilRunnerFailsLoudly(t *testing.T) {
	s := newIterationSession(SessionConfig{})

	code := s.runIterationForProjects(context.Background(), s.cfg.Workspace.Projects, nil)

	if code != protocolcli.ExitFailure {
		t.Fatalf("nil runner exit = %d, want %d (ExitFailure)", code, protocolcli.ExitFailure)
	}
}

// Iteration exit codes are the protocol/cli taxonomy. A failed iteration used
// to return a raw 2, which is ExitUsage — "you used the CLI wrong" — for a
// build that merely failed.
func TestRunIterationForProjects_ExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		outcome IterationResult
		want    int
		chrome  string
	}{
		{"success", IterationResult{ExitCode: protocolcli.ExitSuccess}, protocolcli.ExitSuccess, "done"},
		{"failure", IterationResult{ExitCode: protocolcli.ExitFailure}, protocolcli.ExitFailure, "failed"},
		{
			"aborted keeps the session code for the caller",
			IterationResult{ExitCode: protocolcli.ExitSignal, Aborted: true},
			protocolcli.ExitSuccess,
			"interrupted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{outcome: tc.outcome}
			out := &syncBuffer{}
			s := newIterationSession(SessionConfig{RunIteration: runner.run})
			s.watchRend = NewWatchRenderer(&fakeRenderer{}, out, false)

			code := s.runIterationForProjects(context.Background(), s.cfg.Workspace.Projects, nil)

			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			if code == protocolcli.ExitUsage {
				t.Fatalf("an iteration must never return ExitUsage (%d)", protocolcli.ExitUsage)
			}
			if !strings.Contains(out.String(), tc.chrome) {
				t.Fatalf("watch chrome = %q, want it to mention %q", out.String(), tc.chrome)
			}
		})
	}
}

func TestWaitForWorkspaceQuiet_ReturnsOnContextCancel(t *testing.T) {
	tmp := t.TempDir()
	w := NewWatcher(WatcherConfig{
		Roots:        []string{tmp},
		PollInterval: 5 * time.Millisecond,
	})
	if err := w.TakeSnapshot(); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	s := &Session{watcher: w}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	s.waitForWorkspaceQuiet(ctx, 300*time.Millisecond, 2*time.Second)
	elapsed := time.Since(started)

	if elapsed >= 100*time.Millisecond {
		t.Fatalf("wait took too long: %s", elapsed)
	}
}

func TestWaitForWorkspaceQuiet_ReturnsAfterQuietPeriod(t *testing.T) {
	tmp := t.TempDir()
	w := NewWatcher(WatcherConfig{
		Roots:        []string{tmp},
		PollInterval: 5 * time.Millisecond,
	})
	if err := w.TakeSnapshot(); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	s := &Session{watcher: w}

	started := time.Now()
	s.waitForWorkspaceQuiet(context.Background(), 30*time.Millisecond, 500*time.Millisecond)
	elapsed := time.Since(started)

	if elapsed < 25*time.Millisecond {
		t.Fatalf("wait returned too early: %s", elapsed)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("wait took too long: %s", elapsed)
	}
}

// A serve on port 0 learns its real port from the ready event, so the release
// wait between restarts must wait on the port the previous iteration's ready
// claim announced, read from the stream the renderer saw, instead of parsing 0
// into the default port 3000, which may belong to an unrelated process and
// would stall every restart for the whole release deadline. A pinned port never
// consults the claim.
func TestWaitForServePortRelease_EphemeralPortWaitsOnTheAnnouncedPort(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "an-ephemeral-port-waits-on-the-announced-port", "the-release-wait-reads-the-announced-port")
	claim := func(port int) jobs.RawJobEvent {
		return readyEventFor(t, runtimeproto.ReadyData{
			Target: runtimeproto.ReadyTargetServer,
			Endpoints: []runtimeproto.ReadyEndpoint{
				{Scheme: runtimeproto.ReadySchemeGRPC, Host: "127.0.0.1", Port: port + 1},
				{Scheme: runtimeproto.ReadySchemeHTTP, Host: "127.0.0.1", Port: port},
			},
		})
	}

	s := newIterationSession(SessionConfig{ServeMode: true, CommandParams: map[string]any{"port": 0}})
	s.watchRend.ResetServeReadySignal()
	if port, ok := s.servePortToRelease(); ok {
		t.Fatalf("port to release = %d before any claim, want none: port 0 names no listener", port)
	}
	s.waitForServePortRelease(context.Background())

	s.watchRend.JobEvent(nil, claim(43120))
	if port, ok := s.servePortToRelease(); !ok || port != 43120 {
		t.Fatalf("port to release = %d, %v, want the announced http port 43120", port, ok)
	}
	// A later claim of the same iteration wins; a workload claim, which names no
	// endpoint, says nothing about where the workload listens.
	s.watchRend.JobEvent(nil, claim(43130))
	s.watchRend.JobEvent(nil, readyEventFor(t, runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetWorkload}))
	if port, _ := s.servePortToRelease(); port != 43130 {
		t.Fatalf("port to release = %d, want the last announced port 43130", port)
	}
	// A new iteration forgets the previous one's port.
	s.watchRend.ResetServeReadySignal()
	if port, ok := s.servePortToRelease(); ok {
		t.Fatalf("port to release = %d after a reset, want none", port)
	}

	for _, pinned := range []any{4310, "4310"} {
		p := newIterationSession(SessionConfig{ServeMode: true, CommandParams: map[string]any{"port": pinned}})
		p.watchRend.ResetServeReadySignal()
		p.watchRend.JobEvent(nil, claim(43120))
		if port, ok := p.servePortToRelease(); !ok || port != 4310 {
			t.Errorf("pinned %#v: port to release = %d, %v, want the pinned 4310", pinned, port, ok)
		}
	}
	if port, ok := newIterationSession(SessionConfig{ServeMode: true}).servePortToRelease(); !ok || port != DefaultPort {
		t.Errorf("no port param: port to release = %d, %v, want DefaultPort", port, ok)
	}
}

func TestIsEphemeralPort(t *testing.T) {
	for _, raw := range []any{0, int64(0), float64(0), "0", " 0 "} {
		if !isEphemeralPort(raw) {
			t.Errorf("isEphemeralPort(%#v) = false", raw)
		}
	}
	for _, raw := range []any{3000, "3000", "", nil, true} {
		if isEphemeralPort(raw) {
			t.Errorf("isEphemeralPort(%#v) = true", raw)
		}
	}
}

func TestParsePortValue(t *testing.T) {
	tests := []struct {
		name     string
		raw      any
		fallback int
		want     int
	}{
		{name: "string", raw: "8081", fallback: 3000, want: 8081},
		{name: "int", raw: 8082, fallback: 3000, want: 8082},
		{name: "int64", raw: int64(8083), fallback: 3000, want: 8083},
		{name: "float64", raw: float64(8084), fallback: 3000, want: 8084},
		{name: "invalid", raw: true, fallback: 3000, want: 3000},
	}

	for _, tt := range tests {
		got := parsePortValue(tt.raw, tt.fallback)
		if got != tt.want {
			t.Fatalf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}
