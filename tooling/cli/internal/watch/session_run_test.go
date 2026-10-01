package watch

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// syncBuffer is an io.Writer safe for concurrent writes (from the renderer)
// and reads (from a polling test goroutine) under the race detector.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeRenderer records delegate calls from WatchRenderer.
type fakeRenderer struct {
	starts       int
	jobStarts    int
	jobCompletes int
	finishes     int
}

func (f *fakeRenderer) Start(_ []*jobs.ScheduledJob)                      { f.starts++ }
func (f *fakeRenderer) JobStart(_ *jobs.ScheduledJob)                     { f.jobStarts++ }
func (f *fakeRenderer) JobEvent(_ *jobs.ScheduledJob, _ jobs.RawJobEvent) {}
func (f *fakeRenderer) JobComplete(_ *jobs.ScheduledJob, _ *jobs.JobResult) {
	f.jobCompletes++
}
func (f *fakeRenderer) Finish(_ map[string]*jobs.JobResult, _ jobs.SessionOutcome) { f.finishes++ }

// newLoopTestSession builds a Session whose iterations are a scripted no-op
// runner, so Run drives the full control flow — snapshot, classify, replan,
// quiescence — without an engine or any real subprocess. The watcher polls a temp
// dir so file changes can be injected.
func newLoopTestSession(t *testing.T, tmp string, out io.Writer, serveMode bool) (*Session, *recordingRunner) {
	t.Helper()
	w := NewWatcher(WatcherConfig{
		Roots:        []string{tmp},
		PollInterval: 5 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
	})
	runner := &recordingRunner{}
	return &Session{
		cfg: SessionConfig{
			Workspace:    makeTestWorkspace(),
			Renderer:     &fakeRenderer{},
			RunIteration: runner.run,
			ServeMode:    serveMode,
		},
		watcher:   w,
		watchRend: NewWatchRenderer(&fakeRenderer{}, out, serveMode),
	}, runner
}

func TestSessionRun_NonServe_DetectsChangeAndReplans(t *testing.T) {
	tmp := t.TempDir()
	appSrc := filepath.Join(tmp, "packages", "app", "src")
	if err := os.MkdirAll(appSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out := &syncBuffer{}
	s, runner := newLoopTestSession(t, tmp, out, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- s.Run(ctx) }()

	// Create a file in the app project until the change flows through detect ->
	// classify -> re-plan. The write is retried under a new filename because a
	// file created before Run takes its baseline snapshot is part of that
	// baseline, not a change. The bound is a hang detector, not a latency budget.
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 0; !strings.Contains(out.String(), "affected:"); attempt++ {
		if time.Now().After(deadline) {
			t.Fatalf("a change in the app project never reached a replan\noutput:\n%s", out.String())
		}
		name := filepath.Join(appSrc, "index-"+strconv.Itoa(attempt)+".ts")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()

	exit := <-done
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if s.iteration < 1 {
		t.Errorf("iteration = %d, want >= 1 (change should have triggered a re-run)", s.iteration)
	}
	got := out.String()
	if !strings.Contains(got, "[watch] starting") {
		t.Errorf("missing initial iteration output:\n%s", got)
	}
	if !strings.Contains(got, "change detected") {
		t.Errorf("expected re-run on change:\n%s", got)
	}
	if !strings.Contains(got, "affected:") {
		t.Errorf("expected affected-projects line:\n%s", got)
	}

	// The replan reaches the engine seam, and it reaches it with the classified
	// projects — the initial run for the configured selection, then the projects
	// the changed file maps to.
	calls := runner.snapshot()
	if len(calls) < 2 {
		t.Fatalf("engine seam called %d times, want an initial run plus a replan: %v", len(calls), calls)
	}
	replan := calls[len(calls)-1]
	if len(replan) != 1 || replan[0] != "app" {
		t.Errorf("replan ran for %v, want [app] (the project owning the changed file)", replan)
	}
}

func TestSessionRun_ServeMode_StartsAndStopsOnCancel(t *testing.T) {
	tmp := t.TempDir()
	out := &syncBuffer{}
	s, _ := newLoopTestSession(t, tmp, out, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case exit := <-done:
		if exit != 0 {
			t.Errorf("serve exit = %d, want 0", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve loop did not stop within 3s of cancel")
	}
	if !strings.Contains(out.String(), "[watch] stopped") {
		t.Errorf("expected shutdown output, got:\n%s", out.String())
	}
}

func TestWatchRenderer_DelegatesToBase(t *testing.T) {
	fake := &fakeRenderer{}
	r := NewWatchRenderer(fake, io.Discard, false)
	job := &jobs.ScheduledJob{
		Project: &workspace.Project{ID: "/p", Name: "p"},
		JobDef:  &extension.JobDefinition{Name: "build"},
	}

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "success"})
	r.Finish(map[string]*jobs.JobResult{"/p:build": {Status: "success"}}, jobs.SessionOutcome{})

	if fake.starts != 1 || fake.jobStarts != 1 || fake.jobCompletes != 1 || fake.finishes != 1 {
		t.Errorf("delegate counts = %+v, want each 1", fake)
	}
}

func TestRunWatch_NonServe_StopsOnCancel(t *testing.T) {
	tmp := t.TempDir()
	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
	}
	ws := workspace.NewWorkspace(tmp, nil, projects)
	ws.Name = "ws"
	runner := &recordingRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- RunWatch(ctx, SessionConfig{
			Workspace:        ws,
			SelectedProjects: projects,
			Renderer:         &fakeRenderer{},
			RunIteration:     runner.run,
		})
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case exit := <-done:
		if exit != 0 {
			t.Errorf("RunWatch exit = %d, want 0", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWatch did not stop within 3s of cancel")
	}
}

func TestWaitForServePortRelease_AvailablePortReturnsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // free the port

	s := &Session{cfg: SessionConfig{CommandParams: map[string]any{"port": port}}}

	start := time.Now()
	s.waitForServePortRelease(context.Background())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("expected fast return for available port, took %s", elapsed)
	}
}

func TestWaitForServePortRelease_CanceledWhileBusy(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port // held open -> unavailable

	s := &Session{cfg: SessionConfig{CommandParams: map[string]any{"port": port}}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	s.waitForServePortRelease(ctx)
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("expected return shortly after cancel, took %s", elapsed)
	}
}
