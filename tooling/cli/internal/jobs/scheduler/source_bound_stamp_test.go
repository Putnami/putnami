package scheduler

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// sourceBoundStampRequirement is the requirement the stamp and turn-taking
// tests prove.
const sourceBoundStampRequirement = "source-bound-stamp"

// A stamp write replaces the file whole. A reader that opened the stamp before
// the write keeps reading the complete earlier document, the written document
// is complete, and no temporary file is left beside it. A write in place would
// truncate the file under the reader.
func TestVersionStampWriteReplacesTheFileWhole(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundStampRequirement, "a-stamp-write-replaces-the-stamp-whole")
	dir := filepath.Join(t.TempDir(), ".gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "version.json")
	earlier := VersionInfo{Name: "app", Version: "0.0.0", BuildTime: "earlier", CapabilityPackages: []CapabilityPackageStamp{
		{Package: "app", SourceBinding: "source-v1:sha256:" + strings.Repeat("a", 64)},
	}}
	if err := writeVersionStamp(path, earlier); err != nil {
		t.Fatalf("write the earlier stamp: %v", err)
	}
	earlierBytes := readFileAt(t, path)

	var reader *os.File
	if runtime.GOOS != "windows" {
		// Windows refuses to rename over a file another handle holds open.
		var err error
		if reader, err = os.Open(path); err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
	}
	later := earlier
	later.BuildTime = "later"
	later.CapabilityPackages = []CapabilityPackageStamp{
		{Package: "app", SourceBinding: "source-v1:sha256:" + strings.Repeat("b", 64)},
	}
	if err := writeVersionStamp(path, later); err != nil {
		t.Fatalf("write the later stamp: %v", err)
	}

	if reader != nil {
		read, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(read) != earlierBytes {
			t.Fatalf("a reader that opened the stamp before the write read %q, want the whole earlier stamp %q", read, earlierBytes)
		}
	}
	var got VersionInfo
	if err := json.Unmarshal([]byte(readFileAt(t, path)), &got); err != nil {
		t.Fatalf("the written stamp does not decode: %v", err)
	}
	if got.BuildTime != "later" || got.CapabilityPackages[0].SourceBinding != later.CapabilityPackages[0].SourceBinding {
		t.Fatalf("stamp = %+v, want the later stamp", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"version.json"}) {
		t.Fatalf(".gen holds %v after the write, want only version.json", names)
	}
}

// The preparation of a source-bound task rewrites its project's stamp, so it
// takes the project's task-output key whatever this session already stamped.
// Its subprocess reads the stamp, so it keeps the key until its capture even
// when it declares no read of the project: the key is exclusive from the
// rewrite to the capture, and another session's source-bound task of the
// project waits for both. A task that is not source-bound takes neither once
// the session stamped the project.
func TestPreparationOfASourceBoundTaskTakesItsProjectKey(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundStampRequirement, "a-source-bound-preparation-takes-its-project-key")
	t.Parallel()
	s := &Scheduler{versionFiles: map[string]bool{"proj": true}}
	bound := outputLockJob("proj", "build~describe", nil)
	bound.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true, SourceBound: true}
	if !s.preparationWritesProjectTree(bound) {
		t.Fatal("a source-bound task does not lock its project for the stamp its preparation rewrites")
	}
	if got := taskReadLockNames(bound); !slices.Equal(got, []string{projectOutputLockName(bound)}) {
		t.Fatalf("a source-bound task holds %v until its capture, want its project key for the stamp its subprocess reads", got)
	}
	plain := outputLockJob("proj", "build~compile", nil)
	plain.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true}
	if s.preparationWritesProjectTree(plain) {
		t.Fatal("a task that is not source-bound locks its project although the session already stamped it")
	}
	if got := taskReadLockNames(plain); len(got) != 0 {
		t.Fatalf("a task that reads nothing holds %v until its capture, want no key", got)
	}
}

// sourceBoundTurnScript is a describe script that records, in log, whether
// another describe of the same project was running when it started, holds the
// project for a second, then writes the evidence the way the fixture's
// describe does.
func sourceBoundTurnScript(t *testing.T, running, log string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "describe.sh")
	writeExecutable(t, script, "#!/bin/sh\n"+
		"if mkdir "+shellQuote(running)+" 2>/dev/null; then printf 'alone\\n' >> "+shellQuote(log)+"; "+
		"else printf 'overlap\\n' >> "+shellQuote(log)+"; fi\n"+
		"sleep 1\n"+
		"mkdir -p .gen/schema/feature-evidence schema/feature-evidence\n"+
		"cp .gen/version.json .gen/schema/feature-evidence/go-framework.json\n"+
		"cp .gen/schema/feature-evidence/go-framework.json "+committedFeatureEvidence+"\n"+
		"rmdir "+shellQuote(running)+" 2>/dev/null\n"+
		sourceBoundEvents+"exit 0\n")
	return script
}

// secondSourceBoundDescribe returns a source-bound describe of the same
// project as describeJob, under another command, the way a test --race plans
// a describe beside a build's.
func secondSourceBoundDescribe(describeJob *ScheduledJob, command string) *ScheduledJob {
	commandName, step := jobCommandAndStep("test~describe")
	def := *describeJob.JobDef
	def.Name, def.CommandName, def.StepID, def.Command = "test~describe", commandName, step, command
	return &ScheduledJob{
		Project:   describeJob.Project,
		Extension: describeJob.Extension,
		Step:      &extension.PipelineStep{ID: step, Task: describeJob.Step.Task},
		JobDef:    &def,
	}
}

// Two source-bound tasks of one project in one session, ready together with
// room to run in parallel, take turns: neither starts while the other runs,
// so neither rewrites the stamp while the other executes or captures. Each
// entry is stored at the binding the evidence names.
func TestSourceBoundTasksOfOneProjectTakeTurns(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundStampRequirement, "the-source-bound-tasks-of-one-project-take-turns")
	requireShell(t)
	logs := t.TempDir()
	ws, planned := sourceBoundRunFixture(t, filepath.Join(logs, "describe"), filepath.Join(logs, "compile"))
	running, turns := filepath.Join(logs, "running"), filepath.Join(logs, "turns")
	planned[0].JobDef.Command = sourceBoundTurnScript(t, running, turns)
	planned = []*ScheduledJob{planned[0], secondSourceBoundDescribe(planned[0], sourceBoundTurnScript(t, running, turns))}
	cache := sharedRuntimeCache(t)

	result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 2}, cache)
	for i := range planned {
		if got := resultOf(t, result, planned, i); got.Status != "success" {
			t.Fatalf("%s = %+v, want success", planned[i].Key(), got)
		}
	}
	if got := strings.Fields(readFileAt(t, turns)); !slices.Equal(got, []string{"alone", "alone"}) {
		t.Fatalf("describe starts = %v, want two that each ran alone", got)
	}
	evidence := sourceBoundEvidenceBinding(t, filepath.Join(ws.Root, "app"))
	for i := range planned {
		key := resultOf(t, result, planned, i).CacheKey
		if entry, err := cache.LookupTaskEntry(cachekey.SourceBoundEntryKey(key, evidence)); err != nil || entry == nil {
			t.Fatalf("%s stored no entry at the binding its evidence names, %q: %+v, %v", planned[i].Key(), evidence, entry, err)
		}
	}
}

// countSourceBindings makes s count, per project root, the bindings its memo
// computes.
func countSourceBindings(s *Scheduler) map[string]int {
	calls := make(map[string]int)
	resolve := s.capabilitySourceBindings.resolve
	s.capabilitySourceBindings.resolve = func(repoRoot, projectRoot string) (string, int, error) {
		calls[filepath.Clean(projectRoot)]++
		return resolve(repoRoot, projectRoot)
	}
	return calls
}

// A restore whose project-rooted outputs all lie in paths source-v1 excludes,
// such as a describe hit restoring .gen/schema and the committed feature
// evidence, cannot change the project's binding, so the memo keeps it. A
// restore of a path the binding covers still drops it.
func TestRestoreOfPathsTheBindingExcludesKeepsTheBinding(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundStampRequirement, "a-restore-of-paths-the-binding-excludes-keeps-the-binding")
	ws := makeExecutorTestWorkspace(t)
	runVersionsGit(t, ws.Root, "init")
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws.Projects = []*workspace.Project{project}
	projectRoot := filepath.Join(ws.Root, project.Path)
	writeFileAt(t, filepath.Join(projectRoot, "main.go"), "package main\n")
	s := &Scheduler{ws: ws, capabilitySourceBindings: newCapabilitySourceBindingMemo()}
	calls := countSourceBindings(s)
	job := &ScheduledJob{Project: project}
	output := func(id, kind, path string) store.TaskEntryOutput {
		return store.TaskEntryOutput{ID: id, Kind: kind, Root: extension.OutputRootProject, Path: path, State: store.TaskOutputPresent}
	}
	describeHit := &store.TaskEntry{Outputs: []store.TaskEntryOutput{
		output("schema", extension.OutputKindDirectory, ".gen/schema"),
		output("featureEvidence", extension.OutputKindFile, committedFeatureEvidence),
	}}

	binding := func() string {
		t.Helper()
		result, _ := s.capabilitySourceBindings.sourceBinding(ws.Root, projectRoot)
		if result.err != nil {
			t.Fatalf("compute the binding: %v", result.err)
		}
		return result.binding
	}
	seeded := binding()
	s.invalidateCapabilitySourceBindingsForRestore(job, describeHit)
	writeFileAt(t, filepath.Join(projectRoot, filepath.FromSlash(committedFeatureEvidence)), "{}\n")
	s.invalidateCapabilitySourceBindingsForRestore(job, describeHit)
	if got := binding(); got != seeded || calls[projectRoot] != 1 {
		t.Fatalf("after a restore of excluded paths: binding %q (seeded %q), computed %d times, want the memoized binding computed once", got, seeded, calls[projectRoot])
	}

	covered := &store.TaskEntry{Outputs: []store.TaskEntryOutput{output("openapi", extension.OutputKindFile, "schema/openapi.json")}}
	s.invalidateCapabilitySourceBindingsForRestore(job, covered)
	writeFileAt(t, filepath.Join(projectRoot, "schema", "openapi.json"), "{}\n")
	s.invalidateCapabilitySourceBindingsForRestore(job, covered)
	if got := binding(); got == seeded || calls[projectRoot] != 2 {
		t.Fatalf("after a restore of a covered path: binding %q (seeded %q), computed %d times, want a recomputed, moved binding", got, seeded, calls[projectRoot])
	}
}

// A warm run whose two source-bound describes of one project both hit
// computes the project's binding once: the first hit restores only paths the
// binding excludes, so the second lookup reads the memo.
func TestWarmSourceBoundHitsComputeTheBindingOnce(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundStampRequirement, "a-restore-of-paths-the-binding-excludes-keeps-the-binding")
	requireShell(t)
	logs := t.TempDir()
	describeLog := filepath.Join(logs, "describe")
	ws, planned := sourceBoundRunFixture(t, describeLog, filepath.Join(logs, "compile"))
	planned = append(planned, secondSourceBoundDescribe(planned[0], planned[0].JobDef.Command))
	cache := sharedRuntimeCache(t)
	projectRoot := filepath.Join(ws.Root, "app")

	run := func(name string) map[string]int {
		t.Helper()
		s := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
		calls := countSourceBindings(s)
		result := s.Run(t.Context())
		for i := range planned {
			if got := resultOf(t, result, planned, i); got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
		}
		return calls
	}
	run("cold")
	executed := subprocessRuns(t, describeLog)
	calls := run("warm")
	if got := subprocessRuns(t, describeLog); got != executed {
		t.Fatalf("warm run executed describe %d times, want every describe served from the cache", got-executed)
	}
	if got := calls[projectRoot]; got != 1 {
		t.Fatalf("warm run computed the project's binding %d times, want 1", got)
	}
}
