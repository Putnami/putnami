package watch

import (
	"context"
	"os"
	"path/filepath"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// IterationResult is the verdict of ONE replanned run.
//
// It is deliberately tiny: the loop decides when to re-run, not what a run
// means. Everything richer — the plan, the canonical session, the job results —
// belongs to whoever implements RunIteration.
type IterationResult struct {
	// ExitCode is the run's exit code in the go.putnami.dev/protocol/cli
	// taxonomy. The adapter that produced it has already normalized it, so the
	// loop only ever compares it against protocolcli.ExitSuccess.
	ExitCode int
	// Aborted reports that the run was cut short — a signal, or the serve loop's
	// own restart cancel — before it could be either a success or a failure.
	Aborted bool
}

// RunIteration executes one watch iteration for the given projects, rendering
// through sink, and reports the verdict.
//
// It is the ENGINE SEAM. Watch is a trigger and replan policy: it decides
// WHEN to run and over WHICH projects, and the engine owns
// what a run is — workspace and extension resolution, planning, contract
// validation, the missing-extension guards, cache setup, scheduling, the session
// file and the exit code. internal/engine binds this to Engine.Run and cannot be
// imported from here: the engine already imports this package to enter the loop,
// so a direct call back would close an import cycle. The callback is the same
// inversion commands.LifecycleEnv.RunJob uses for `putnami install`.
type RunIteration func(ctx context.Context, projects []*workspace.Project, sink jobs.Renderer) IterationResult

// SessionConfig holds all configuration for a watch session.
type SessionConfig struct {
	// Workspace is the loaded workspace the change classification maps against.
	Workspace *workspace.Workspace
	// SelectedProjects are the projects the INITIAL iteration runs for. Later
	// iterations run for the projects the changed files map to.
	SelectedProjects []*workspace.Project
	// CommandParams are the run's job params. The loop reads exactly one of them,
	// "port", to wait for a serve port to be released between restarts.
	CommandParams map[string]any
	// WorkspaceInputPatterns are the workspace-scoped task input patterns
	// (lockfiles, root compiler config) that make a change affect every project.
	// They are the one thing the shared change→project mapping needs that a git
	// diff does not carry; see workspace.ChangeImpactOptions.
	WorkspaceInputPatterns []string
	// Renderer is the run's base renderer, wrapped with the watch chrome.
	Renderer jobs.Renderer
	// RunIteration runs one iteration. A nil runner fails the session loudly
	// rather than turning watch into a silent no-op that never rebuilds.
	RunIteration RunIteration
	// ServeMode selects the serve loop (process lifecycle, readiness gating,
	// port-release waits) over the plain re-run loop. It is passed in rather than
	// re-derived from the commands: the engine already decided this fact when it
	// force-enabled watch for serve, and deriving it twice let the two disagree.
	ServeMode bool
	// BoundPort reports the port the served process last bound, for a run whose
	// "port" param is 0 (an ephemeral port the workload picks and announces in
	// its ready event). The loop waits for THAT port to be released between
	// restarts, since port 0 names no listener. Nil keeps the param-derived
	// port, which is every serve that pins its port.
	BoundPort func() int
}

// Session manages the watch loop: initial run, file watching,
// change classification, re-planning, and re-execution.
type Session struct {
	cfg       SessionConfig
	watcher   *Watcher
	watchRend *WatchRenderer
	iteration int
}

// NewSession creates a watch session.
func NewSession(cfg SessionConfig) *Session {
	ws := cfg.Workspace

	// Set up file watcher on all project directories + workspace root
	watchRoots := []string{ws.Root}

	watcher := NewWatcher(WatcherConfig{
		Roots:    watchRoots,
		Debounce: DefaultDebounce,
	})

	watchRend := NewWatchRenderer(cfg.Renderer, os.Stderr, cfg.ServeMode)

	return &Session{
		cfg:       cfg,
		watcher:   watcher,
		watchRend: watchRend,
	}
}

// Run starts the watch session. It blocks until the context is canceled
// (Ctrl+C). Returns the exit code.
func (s *Session) Run(ctx context.Context) int {
	// Take initial file system snapshot
	if err := s.watcher.TakeSnapshot(); err != nil {
		iox.Fprintf(os.Stderr, "putnami: watch: snapshot failed: %v\n", err)
		return protocolcli.ExitFailure
	}

	if s.cfg.ServeMode {
		return s.runServeLoop(ctx)
	}

	// Initial run (non-serve)
	s.watchRend.IterationStart(0, nil)
	s.runIteration(ctx, nil)

	return s.runJobLoop(ctx)
}

// RunWatch is the top-level entry point for --watch mode: it creates and runs a
// watch session. The engine calls it after planning the initial run, and every
// iteration goes back through Engine.Run via cfg.RunIteration.
func RunWatch(ctx context.Context, cfg SessionConfig) int {
	// Create output directory for watch
	outDir := filepath.Join(cfg.Workspace.Root, ".putnami", "out")
	_ = os.MkdirAll(outDir, 0o755)

	return NewSession(cfg).Run(ctx)
}
