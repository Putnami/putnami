package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func withFakeHookRunner(
	t *testing.T,
	fn func(context.Context, *workspace.Workspace, *extension.ExtensionDescription, *workspace.Project, bool, []string) (*hooks.HookResult, error),
) {
	t.Helper()
	orig := runPreBuildHookFunc
	t.Cleanup(func() { runPreBuildHookFunc = orig })
	runPreBuildHookFunc = fn
}

func jobWithHook(projName string) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{
			ID:   "/" + projName,
			Name: projName,
			Path: projName,
		},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/web",
			Path: "/ext/web",
			Hooks: &extension.ManifestHooks{
				PreBuild: &extension.HookDefinition{Kind: "command", Command: "bun"},
			},
		},
		JobDef: &extension.JobDefinition{Name: "build"},
	}
}

func newTestScheduler() *Scheduler {
	return &Scheduler{
		ws:        &workspace.Workspace{Root: "/ws"},
		hookSlots: make(map[string]*hookSlot),
	}
}

// TestRunPreBuildHook_DedupesConcurrentCallers verifies that N concurrent
// callers with the same extension+project key invoke the underlying hook
// exactly once, and that followers BLOCK on the leader instead of skipping
// past while the hook output is still being written. This is the core race
// fix — the old map[string]bool would let followers proceed immediately.
func TestRunPreBuildHook_DedupesConcurrentCallers(t *testing.T) {
	s := newTestScheduler()
	job := jobWithHook("proj-a")

	const concurrency = 8
	var calls atomic.Int32
	hookStart := make(chan struct{})
	hookRelease := make(chan struct{})

	withFakeHookRunner(t, func(ctx context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		calls.Add(1)
		close(hookStart)
		<-hookRelease
		return &hooks.HookResult{}, nil
	})

	type res struct {
		err     error
		stopped time.Time
	}
	results := make(chan res, concurrency)
	var ready sync.WaitGroup
	ready.Add(concurrency)
	go_ := func() {
		ready.Done()
		err := s.runPreBuildHook(context.Background(), job)
		results <- res{err: err, stopped: time.Now()}
	}

	for i := 0; i < concurrency; i++ {
		go go_()
	}
	ready.Wait()

	// Wait for leader to enter the hook (proves at least one goroutine got the slot)
	select {
	case <-hookStart:
	case <-time.After(answerWaitBudget):
		t.Fatal("leader never entered hook — dedup may have skipped everyone")
	}

	// Followers must be blocked, not skipping past. Give them a chance to run.
	time.Sleep(50 * time.Millisecond)
	if len(results) != 0 {
		t.Fatalf("expected followers to block on leader; got %d early returns", len(results))
	}

	// Release the leader and collect results.
	close(hookRelease)
	for i := 0; i < concurrency; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("caller %d got unexpected error: %v", i, r.err)
			}
		case <-time.After(answerWaitBudget):
			t.Fatalf("only %d/%d callers returned after the leader was released", i, concurrency)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("expected hook to be invoked exactly once, got %d", got)
	}
}

// TestRunPreBuildHook_FailurePropagatesToAllWaiters verifies that when the
// hook fails, every waiter sees the same error rather than silently
// skipping. The previous code marked done=true unconditionally — subsequent
// callers would get a nil error and proceed without the hook outputs,
// hiding the real failure and corrupting downstream builds.
func TestRunPreBuildHook_FailurePropagatesToAllWaiters(t *testing.T) {
	s := newTestScheduler()
	job := jobWithHook("proj-b")

	wantErr := errors.New("hook crashed: bundle failed")
	withFakeHookRunner(t, func(ctx context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		time.Sleep(20 * time.Millisecond)
		return nil, wantErr
	})

	const concurrency = 5
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() { errs <- s.runPreBuildHook(context.Background(), job) }()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case got := <-errs:
			if !errors.Is(got, wantErr) {
				t.Errorf("caller %d: expected %v, got %v", i, wantErr, got)
			}
		case <-time.After(answerWaitBudget):
			t.Fatalf("caller %d never received the leader's hook error", i)
		}
	}
}

// TestRunPreBuildHook_DifferentKeysRunInParallel verifies that
// different extension+project pairs do NOT serialize. Each pair must run
// concurrently — the dedup is per-key, not global.
func TestRunPreBuildHook_DifferentKeysRunInParallel(t *testing.T) {
	s := newTestScheduler()
	jobA := jobWithHook("proj-a")
	jobB := jobWithHook("proj-b")

	inFlight := make(chan struct{}, 2)
	release := make(chan struct{})
	withFakeHookRunner(t, func(ctx context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		inFlight <- struct{}{}
		<-release
		return &hooks.HookResult{}, nil
	})

	go func() { _ = s.runPreBuildHook(context.Background(), jobA) }()
	go func() { _ = s.runPreBuildHook(context.Background(), jobB) }()

	// Both jobs should enter the hook concurrently — if dedup is overly broad,
	// only one would enter and the second would be blocked.
	for i := 0; i < 2; i++ {
		select {
		case <-inFlight:
		case <-time.After(answerWaitBudget):
			t.Fatalf("only %d/2 hooks entered concurrently — dedup is incorrectly cross-key", i)
		}
	}
	close(release)
}

// TestRunPreBuildHook_ContextCancellationUnblocksFollower verifies that a
// follower whose context is canceled does NOT wait forever on a slow leader.
func TestRunPreBuildHook_ContextCancellationUnblocksFollower(t *testing.T) {
	s := newTestScheduler()
	job := jobWithHook("proj-c")

	leaderEntered := make(chan struct{})
	hold := make(chan struct{})
	withFakeHookRunner(t, func(ctx context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		close(leaderEntered)
		<-hold
		return &hooks.HookResult{}, nil
	})

	// Leader: a long-lived run.
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- s.runPreBuildHook(context.Background(), job) }()
	<-leaderEntered

	// Follower with a cancellable context.
	followerCtx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan error, 1)
	go func() { followerDone <- s.runPreBuildHook(followerCtx, job) }()

	// Give the follower a moment to enter the wait.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-followerDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(answerWaitBudget):
		t.Fatal("follower did not unblock on context cancellation")
	}

	// Cleanup: release the leader so the goroutine finishes.
	close(hold)
	<-leaderDone
}

// TestRunPreBuildHook_NoHookDefined verifies the fast path when an extension
// declares no preBuild hook — must return nil without touching the slot map.
func TestRunPreBuildHook_NoHookDefined(t *testing.T) {
	s := newTestScheduler()
	job := jobWithHook("proj-d")
	job.Extension.Hooks = nil

	called := false
	withFakeHookRunner(t, func(ctx context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		called = true
		return &hooks.HookResult{}, nil
	})

	if err := s.runPreBuildHook(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("hook runner was invoked for extension with no preBuild hook")
	}
	if len(s.hookSlots) != 0 {
		t.Errorf("expected no slots to be created, got %d", len(s.hookSlots))
	}
}

// The jobs of a hosted install's dependency fetch run no
// preBuild hook, so no repository code runs while one extension's fetch holds
// the job credential. Two fetch jobs whose extensions declare the hook start
// none of it; the same jobs outside the dependency fetch run it once each.
func TestRunPreBuildHook_TheDependencyFetchRunsNoHook(t *testing.T) {
	s := newTestScheduler()
	var ran []string
	withFakeHookRunner(t, func(_ context.Context, _ *workspace.Workspace, ext *extension.ExtensionDescription, project *workspace.Project, _ bool, _ []string) (*hooks.HookResult, error) {
		ran = append(ran, ext.Name+" "+project.Name)
		return &hooks.HookResult{}, nil
	})
	fetch := func(extensionName string) *ScheduledJob {
		job := jobWithHook("proj-fetch")
		job.Extension.Name = extensionName
		job.JobDef = &extension.JobDefinition{Name: "workspace-fetch"}
		return job
	}
	jobs := []*ScheduledJob{fetch("@acme/a"), fetch("@acme/b")}

	for _, job := range jobs {
		if err := s.runPreBuildHook(WithDependencyFetch(context.Background()), job); err != nil {
			t.Fatalf("dependency fetch: %v", err)
		}
	}
	if len(ran) != 0 || len(s.hookSlots) != 0 {
		t.Fatalf("the dependency fetch ran preBuild hooks %q (%d slots), want none", ran, len(s.hookSlots))
	}

	for _, job := range jobs {
		if err := s.runPreBuildHook(context.Background(), job); err != nil {
			t.Fatalf("outside the dependency fetch: %v", err)
		}
	}
	if want := []string{"@acme/a proj-fetch", "@acme/b proj-fetch"}; len(ran) != len(want) || ran[0] != want[0] || ran[1] != want[1] {
		t.Fatalf("outside the dependency fetch the hooks ran as %q, want %q", ran, want)
	}
}

// TestRunPreBuildHook_PassesTheHeldOutputLocks pins that the hook receives the
// task-output lock ids its task holds during preparation, and no entry when
// the task holds none, so a nested session the hook starts re-enters its own
// task's keys.
func TestRunPreBuildHook_PassesTheHeldOutputLocks(t *testing.T) {
	s := newTestScheduler()
	var got [][]string
	withFakeHookRunner(t, func(_ context.Context, _ *workspace.Workspace, _ *extension.ExtensionDescription, _ *workspace.Project, _ bool, env []string) (*hooks.HookResult, error) {
		got = append(got, env)
		return &hooks.HookResult{}, nil
	})

	if err := s.runPreBuildHook(withHeldOutputLocks(context.Background(), "aa,bb"), jobWithHook("proj-held")); err != nil {
		t.Fatalf("hook with held locks: %v", err)
	}
	if err := s.runPreBuildHook(context.Background(), jobWithHook("proj-none")); err != nil {
		t.Fatalf("hook without held locks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the hook ran %d times, want 2", len(got))
	}
	if len(got[0]) != 1 || got[0][0] != heldOutputLocksEnv+"=aa,bb" {
		t.Fatalf("hook env = %q, want only %s=aa,bb", got[0], heldOutputLocksEnv)
	}
	if got[1] != nil {
		t.Fatalf("hook env without held locks = %q, want none", got[1])
	}
}
