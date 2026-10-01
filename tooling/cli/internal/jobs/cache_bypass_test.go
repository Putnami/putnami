package jobs

import (
	"context"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// --no-cache-projects, exercised through the REAL scheduler, REAL subprocesses
// and a REAL store. The subprocess log is the only honest answer to "did this
// task run again", which is the entire claim of the flag.

// scopedBypassFixture plans one cacheable task per project over two projects:
// `/lib` has no dependency, `/app` depends on it. Every invocation appends a
// line to its own log, so a test counts spawns instead of trusting a status.
func scopedBypassFixture(t *testing.T) (*workspace.Workspace, []*ScheduledJob, string, string) {
	t.Helper()
	root := t.TempDir()
	logs := t.TempDir()
	libLog := filepath.Join(logs, "lib")
	appLog := filepath.Join(logs, "app")
	writeTestFile(t, filepath.Join(root, "lib", "main.go"), "package lib\n")
	writeTestFile(t, filepath.Join(root, "app", "main.go"), "package app\n")

	script := func(name, logPath string) string {
		path := filepath.Join(root, name+".sh")
		writeExecutable(t, path,
			"#!/bin/sh\n"+
				"printf '"+name+"\\n' >> "+shellQuote(logPath)+"\n"+
				"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
				"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
				"exit 0\n")
		return path
	}

	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Name = "scoped-bypass-ws"

	ext := &extension.ExtensionDescription{
		Name:    "@test/scoped",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"compile": {Declares: &extension.TaskDeclaration{}},
		},
	}
	newJob := func(project *workspace.Project, command string) *ScheduledJob {
		commandName, step := jobCommandAndStep("build~compile")
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: "compile"},
			JobDef: &extension.JobDefinition{
				ExtensionName:   "@test/scoped",
				Name:            "build~compile",
				CommandName:     commandName,
				StepID:          step,
				Command:         command,
				Cwd:             "{projectRoot}",
				Cache:           true,
				FilePatterns:    []string{"main.go"},
				TaskCachePolicy: &extension.TaskCachePolicy{NoOutput: true},
			},
		}
	}
	planned := []*ScheduledJob{
		newJob(lib, script("lib", libLog)),
		newJob(app, script("app", appLog)),
	}
	planned[1].DependsOn = []string{planned[0].Key()}
	return ws, planned, libLog, appLog
}

// TestScopedCacheBypass_SparesTheDependencyClosure is the flag, end to end: the
// named project is re-executed and publishes nothing, while the dependency it
// was planned with is served from the cache. This scoping exists because
// `--projects <p>` plans p's whole closure, and refusing the cache run-wide
// recompiled all of it for a check that only re-derives p.
func TestScopedCacheBypass_SparesTheDependencyClosure(t *testing.T) {
	t.Parallel()
	for _, check := range []string{
		"only-the-named-projects-and-their-dependents-lose-the-cache",
		"a-bypassed-task-is-neither-served-nor-published",
	} {
		spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass", check)
	}
	requireShell(t)

	ws, planned, libLog, appLog := scopedBypassFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	// One machine-global store, a fresh CacheManager per run: that is what a
	// second `putnami` invocation is, and it keeps this test from reading a
	// memoized file digest a real second process would never have.
	newCache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(storeRoot))
	}
	bypassApp := SchedulerConfig{MaxParallel: 1, NoCacheProjects: map[string]bool{"/app": true}}
	warm := SchedulerConfig{MaxParallel: 1}

	// --- 1. cold, with /app bypassed: both execute. /lib publishes its entry;
	// /app must not, because it was never looked up.
	first := runSharedScheduler(context.Background(), ws, planned, bypassApp, newCache())
	if !first.Success {
		t.Fatalf("cold run failed: %+v", first.Results)
	}
	if got := subprocessRuns(t, libLog); got != 1 {
		t.Fatalf("cold /lib spawns = %d, want 1", got)
	}
	if got := subprocessRuns(t, appLog); got != 1 {
		t.Fatalf("cold /app spawns = %d, want 1", got)
	}

	// --- 2. no bypass: /lib is served from the entry run 1 published, and /app
	// runs again — the run that bypassed it published nothing to serve.
	second := runSharedScheduler(context.Background(), ws, planned, warm, newCache())
	if !second.Success {
		t.Fatalf("second run failed: %+v", second.Results)
	}
	if got := subprocessRuns(t, libLog); got != 1 {
		t.Fatalf("/lib spawned again (%d total): the dependency of a bypassed project must keep its cache", got)
	}
	if got := subprocessRuns(t, appLog); got != 2 {
		t.Fatalf("/app spawns = %d, want 2: a bypassed task must publish nothing for a later run to be served", got)
	}
	if !resultOf(t, second, planned, 0).CacheHit {
		t.Error("/lib did not report a cache hit in the run that allowed one")
	}

	// --- 3. still no bypass: /app is served now, so the miss in run 2 was the
	// bypass and not a key that moves between runs.
	third := runSharedScheduler(context.Background(), ws, planned, warm, newCache())
	if got := subprocessRuns(t, appLog); got != 2 {
		t.Fatalf("/app spawns = %d, want 2: its key is unstable, so the run-2 miss proved nothing", got)
	}
	if !resultOf(t, third, planned, 1).CacheHit {
		t.Error("/app did not report a cache hit once the bypass was gone")
	}

	// --- 4. bypass the DEPENDENCY: /lib re-executes, and so does /app, because a
	// task whose upstream published no key cannot be keyed against it.
	fourth := runSharedScheduler(context.Background(), ws, planned,
		SchedulerConfig{MaxParallel: 1, NoCacheProjects: map[string]bool{"/lib": true}}, newCache())
	if !fourth.Success {
		t.Fatalf("fourth run failed: %+v", fourth.Results)
	}
	if got := subprocessRuns(t, libLog); got != 2 {
		t.Fatalf("/lib spawns = %d, want 2: the named project must re-execute", got)
	}
	if got := subprocessRuns(t, appLog); got != 3 {
		t.Fatalf("/app spawns = %d, want 3: a dependent of a bypassed project must lose the cache too, "+
			"or it is served an entry whose key is blind to what its upstream produced", got)
	}
}

// TestNewCacheBypass_ResolvesTheDependentClosure pins the closure rule on its
// own, including the two edges that must NOT extend it.
func TestNewCacheBypass_ResolvesTheDependentClosure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"only-the-named-projects-and-their-dependents-lose-the-cache")

	job := func(projectID, name string) *ScheduledJob {
		return &ScheduledJob{
			Project: &workspace.Project{ID: projectID, Name: projectID, Path: projectID[1:]},
			JobDef:  &extension.JobDefinition{Name: name, Cache: true},
		}
	}
	seed := job("/lib", "build~compile")
	dependent := job("/app", "build~compile")
	dependent.DependsOn = []string{seed.Key()}
	transitive := job("/site", "build~compile")
	transitive.DependsOn = []string{dependent.Key()}
	// SerializeAfter orders execution and never contributes to a cache key, so
	// a job that merely waits for a bypassed one keeps its cache.
	serialized := job("/tool", "build~compile")
	serialized.SerializeAfter = []string{seed.Key()}
	unrelated := job("/docs", "build~compile")
	planned := []*ScheduledJob{seed, dependent, transitive, serialized, unrelated}

	bypass := NewCacheBypass(planned, false, map[string]bool{"/lib": true})
	for _, excluded := range []*ScheduledJob{seed, dependent, transitive} {
		if !bypass.Excludes(excluded) {
			t.Errorf("%s keeps the cache; it is the named project or downstream of it", excluded.Key())
		}
	}
	for _, kept := range []*ScheduledJob{serialized, unrelated} {
		if bypass.Excludes(kept) {
			t.Errorf("%s lost the cache; only the named projects and their cache-key dependents may", kept.Key())
		}
	}

	// An empty selection bypasses nothing, and --no-cache subsumes the flag.
	if NewCacheBypass(planned, false, nil).Excludes(seed) {
		t.Error("an empty --no-cache-projects bypassed a job")
	}
	if !NewCacheBypass(planned, true, nil).Excludes(unrelated) {
		t.Error("--no-cache did not bypass every job")
	}
	// A project the plan does not contain names nothing, and must not make the
	// bypass silently cover something else.
	if NewCacheBypass(planned, false, map[string]bool{"/absent": true}).Excludes(seed) {
		t.Error("a project outside the plan bypassed an unrelated job")
	}
}
