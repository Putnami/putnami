package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The telemetry seam is an ACCEPTANCE CRITERION, recorded
// as binding in ADR 0001 §4: the engine owns an injected observer, never
// telemetry. If a later slice moves reporting into the engine body, every
// adapter that routes through Engine.Run (MCP, watch, lifecycle) starts
// synthesizing user sessions — a consent incident against the rule "never
// send before notice". These tests fail before that ships, not after.

type recordingObserver struct {
	calls []RunPlan
}

func (o *recordingObserver) RunPlanned(_ context.Context, plan RunPlan) {
	o.calls = append(o.calls, plan)
}

// TestNotifyRunPlanned_NilObserverIsATotalNoOp pins the "no allocation, no
// buffer write" half of the seam: with no observer the engine must not even
// build the observation.
func TestNotifyRunPlanned_NilObserverIsATotalNoOp(t *testing.T) {
	built := 0
	plan := func() RunPlan {
		built++
		return RunPlan{Commands: []string{"build"}, Projects: 1, Jobs: 2}
	}

	notifyRunPlanned(context.Background(), nil, plan)
	if built != 0 {
		t.Fatalf("observation built %d times for a nil observer, want 0", built)
	}

	commands := []string{"build"}
	allocs := testing.AllocsPerRun(50, func() {
		notifyRunPlanned(context.Background(), nil, func() RunPlan {
			return RunPlan{Commands: commands, Projects: 1, Jobs: 2}
		})
	})
	if allocs != 0 {
		t.Fatalf("nil-observer notification allocated %v times per run, want 0", allocs)
	}
}

// TestNotifyRunPlanned_ObserverReceivesTheCounts is the positive control: the
// same call site does hand the plan over when an observer is injected.
func TestNotifyRunPlanned_ObserverReceivesTheCounts(t *testing.T) {
	t.Parallel()
	observer := &recordingObserver{}
	notifyRunPlanned(context.Background(), observer, func() RunPlan {
		return RunPlan{Commands: []string{"lint", "build"}, Projects: 3, Jobs: 7}
	})

	if len(observer.calls) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(observer.calls))
	}
	got := observer.calls[0]
	if got.Projects != 3 || got.Jobs != 7 || strings.Join(got.Commands, ",") != "lint,build" {
		t.Fatalf("observed plan = %+v, want commands lint,build over 3 projects / 7 jobs", got)
	}
}

// TestRun_UnplannedRunIsNeverObserved pins the observation POINT: a run that
// never reaches a plan (here: no workspace) records nothing, which is why the
// pre-A3a code tracked session:start after planning rather than at entry.
func TestRun_UnplannedRunIsNeverObserved(t *testing.T) {
	observer := &recordingObserver{}
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			result, _ := New().Run(context.Background(), Request{
				WorkspaceRoot: t.TempDir(),
				Config:        &wsproto.Config{},
				Commands:      []string{"build"},
				Observer:      observer,
			}, nil)
			if result.ExitCode == ExitSuccess {
				t.Errorf("a workspace-less run must not succeed; got %d", result.ExitCode)
			}
		})
	})
	if len(observer.calls) != 0 {
		t.Fatalf("observer called %d times before a plan existed, want 0", len(observer.calls))
	}
}

// TestEngineNeverImportsTelemetry is the structural half of ADR 0001 §4: the
// engine cannot report telemetry because it cannot reach the package. A slice
// that "just adds a TrackSessionStart here" fails here first.
func TestEngineNeverImportsTelemetry(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine package dir: %v", err)
	}
	const banned = "tooling/cli/internal/telemetry"
	for _, entry := range entries {
		name := entry.Name()
		// Production sources only: this file names the banned path in its own
		// failure message, and a test may legitimately assert against it.
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), banned) {
			t.Errorf("%s references %s: the engine owns the observer SEAM, not telemetry "+
				"(ADR 0001 §4). Report through Request.Observer, which only the terminal "+
				"adapter supplies.", name, banned)
		}
	}
}

// TestOnlyTheTerminalAdapterSuppliesAnObserver pins the other half: exactly one
// production site in the module injects an Observer, and it is the terminal
// adapter. MCP, watch, and lifecycle adapters must keep passing nil; adding a
// second site requires a new ADR and a consent review, never a refactor slice.
func TestOnlyTheTerminalAdapterSuppliesAnObserver(t *testing.T) {
	t.Parallel()
	root := engineModuleRoot(t)
	const wantSite = "internal/cli/jobs_run.go"

	var sites []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".putnami", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "Observer:") {
				sites = append(sites, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(sites)

	if len(sites) != 1 || !strings.HasPrefix(sites[0], wantSite+":") {
		t.Fatalf("engine.Request observer injection sites = %v, want exactly one in %s.\n"+
			"  ADR 0001 §4: only the terminal adapter supplies an observer; every other "+
			"adapter passes nil so routing it through the engine cannot start reporting "+
			"sessions.", sites, wantSite)
	}
}

// engineModuleRoot walks up to the directory holding tooling/cli's go.mod.
func engineModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the test working directory")
		}
		dir = parent
	}
}
