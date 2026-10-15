package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// An executing task starts from an empty owned directory (ADR 0064). The cases
// here run a real generator through the real scheduler, because the claim is
// about what the directory and the stored entry hold after a run, and nothing
// smaller than a run answers it. The generator never deletes a file it wrote
// earlier: that is the tool the engine must not depend on.

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const (
	emptyingFeature     = "cli/job-planning-execution"
	emptyingRequirement = "executed-directory-output-starts-empty"
	emptyingProject     = "web"
	emptyingTask        = "build-generate"
)

// emptyingFixture is one project whose generation task owns the project-rooted
// .gen directory, cedes .gen/migration-bundle to another task and preserves
// .gen/handwritten for the workspace. Its tool writes one page per line of
// pages.txt under .gen/public/static.
type emptyingFixture struct {
	ws        *workspace.Workspace
	planned   []*ScheduledJob
	logPath   string
	storeRoot string
	gen       string
}

// generatorScript is the fixture tool. It logs one word per invocation, naming
// the command it runs under and whether the build stamp was there when it
// started. With failOnce,
// the first invocation writes a page and fails, and later ones succeed.
func generatorScript(logPath, failOnce string) string {
	script := "#!/bin/sh\n" +
		"stamp=absent\n" +
		"if [ -f .gen/version.json ]; then stamp=present; fi\n" +
		"printf 'run:%s:stamp-%s\\n' \"$PUTNAMI_JOB_NAME\" \"$stamp\" >> " + shellQuote(logPath) + "\n" +
		"mkdir -p .gen/public/static\n"
	if failOnce != "" {
		script += "if [ ! -f " + shellQuote(failOnce) + " ]; then\n" +
			"  : > " + shellQuote(failOnce) + "\n" +
			"  printf 'half\\n' > .gen/public/static/half-written.html\n" +
			"  exit 1\n" +
			"fi\n"
	}
	return script +
		"while IFS= read -r page; do\n" +
		"  if [ -n \"$page\" ]; then printf '<h1>%s</h1>\\n' \"$page\" > \".gen/public/static/$page.html\"; fi\n" +
		"done < pages.txt\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"
}

// newEmptyingFixture plans one node per job name over the fixture project, each
// later node serialized after the one before it. failOnce makes the tool fail
// its first invocation.
func newEmptyingFixture(t *testing.T, failOnce bool, jobNames ...string) *emptyingFixture {
	t.Helper()
	requireShell(t)
	root := t.TempDir()
	f := &emptyingFixture{
		logPath:   filepath.Join(t.TempDir(), "invocations"),
		storeRoot: filepath.Join(t.TempDir(), "store"),
		gen:       filepath.Join(root, emptyingProject, ".gen"),
	}
	marker := ""
	if failOnce {
		marker = filepath.Join(t.TempDir(), "failed-once")
	}
	command := filepath.Join(root, "generate.sh")
	writeExecutable(t, command, generatorScript(f.logPath, marker))
	writeTestFile(t, filepath.Join(root, emptyingProject, "pages.txt"), "")

	project := &workspace.Project{ID: "/" + emptyingProject, Name: emptyingProject, Path: emptyingProject, Version: "1.0.0"}
	f.ws = workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	f.ws.Name = "emptying-ws"
	ext := &extension.ExtensionDescription{
		Name:    "@test/generator",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			emptyingTask: {Declares: &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{
					"gen": {
						Kind:      extension.OutputKindDirectory,
						Root:      extension.OutputRootProject,
						Path:      ".gen",
						Excludes:  []string{".gen/migration-bundle"},
						Preserves: []string{"handwritten"},
					},
				},
			}},
		},
	}
	if len(jobNames) == 0 {
		jobNames = []string{"build~generate"}
	}
	for _, name := range jobNames {
		commandName, step := jobCommandAndStep(name)
		job := &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: emptyingTask},
			JobDef: &extension.JobDefinition{
				ExtensionName:   ext.Name,
				Name:            name,
				CommandName:     commandName,
				StepID:          step,
				Command:         command,
				Cwd:             "{projectRoot}",
				Cache:           true,
				FilePatterns:    []string{"pages.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true},
			},
		}
		if n := len(f.planned); n > 0 {
			job.SerializeAfter = []string{f.planned[n-1].Key()}
		}
		f.planned = append(f.planned, job)
	}
	return f
}

// setPages rewrites the generator's one keyed input.
func (f *emptyingFixture) setPages(t *testing.T, pages ...string) {
	t.Helper()
	content := ""
	for _, page := range pages {
		content += page + "\n"
	}
	writeTestFile(t, filepath.Join(f.ws.Root, emptyingProject, "pages.txt"), content)
}

// run is one CLI session over the fixture's plan. The entries persist in the
// store between sessions; the manager, and with it the per-run file memo, is
// new each time, as it is for a new process.
func (f *emptyingFixture) run(t *testing.T, cfg SchedulerConfig) *SchedulerResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if cfg.MaxParallel == 0 {
		cfg.MaxParallel = 1
	}
	cache := store.NewCacheManager(store.NewLocalStore(f.storeRoot))
	return runBatchScheduler(ctx, f.ws, f.planned, cfg, cache)
}

// executed fails unless every node of the session ran its subprocess and
// succeeded, and returns the rows in plan order.
func (f *emptyingFixture) executed(t *testing.T, leg string, result *SchedulerResult) []*JobResult {
	t.Helper()
	rows := f.rows(t, leg, result)
	for i, row := range rows {
		if row.Status != "success" || row.CacheHit {
			t.Fatalf("%s: %s = status %q hit %v, want an execution that succeeded (error: %+v)",
				leg, f.planned[i].Key(), row.Status, row.CacheHit, row.Error)
		}
	}
	return rows
}

// hit fails unless every node of the session was served from the store.
func (f *emptyingFixture) hit(t *testing.T, leg string, result *SchedulerResult) []*JobResult {
	t.Helper()
	rows := f.rows(t, leg, result)
	for i, row := range rows {
		if row.Status != "success" || !row.CacheHit {
			t.Fatalf("%s: %s = status %q hit %v, want a cache hit (error: %+v)",
				leg, f.planned[i].Key(), row.Status, row.CacheHit, row.Error)
		}
	}
	return rows
}

func (f *emptyingFixture) rows(t *testing.T, leg string, result *SchedulerResult) []*JobResult {
	t.Helper()
	rows := make([]*JobResult, 0, len(f.planned))
	for _, job := range f.planned {
		row := result.Results[job.Key()]
		if row == nil {
			t.Fatalf("%s: no row for %s", leg, job.Key())
		}
		rows = append(rows, row)
	}
	return rows
}

// storedFiles lists what the entry published under key holds for the gen
// output: the files a later hit restores.
func (f *emptyingFixture) storedFiles(t *testing.T, key string) []string {
	t.Helper()
	return f.storedOutput(t, key, "gen")
}

// storedOutput lists what the entry published under key holds for the output
// id.
func (f *emptyingFixture) storedOutput(t *testing.T, key, id string) []string {
	t.Helper()
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(key)
	if err != nil || entry == nil {
		t.Fatalf("no task-owned entry under %s: entry=%v err=%v", key, entry, err)
	}
	return filesIn(t, filepath.Join(entry.FilesDir, id))
}

// filesIn lists the files under root, slash-form and sorted. An absent root
// holds none.
func filesIn(t *testing.T, root string) []string {
	t.Helper()
	files := []string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

func assertFilesIn(t *testing.T, leg, root string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := filesIn(t, root); !reflect.DeepEqual(got, append([]string{}, want...)) {
		t.Fatalf("%s: %s holds %v, want %v", leg, root, got, want)
	}
}

// recordEmptyings wraps the emptying for the rest of the test and returns the
// destinations it was asked to empty so far. Not for parallel tests: it swaps a
// package variable.
func recordEmptyings(t *testing.T) func() []string {
	t.Helper()
	saved := emptyTaskOutput
	var mu sync.Mutex
	var emptied []string
	emptyTaskOutput = func(dest, stagingRoot string, keep ...string) error {
		mu.Lock()
		emptied = append(emptied, dest)
		mu.Unlock()
		return saved(dest, stagingRoot, keep...)
	}
	t.Cleanup(func() { emptyTaskOutput = saved })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), emptied...)
	}
}

// engineWithoutEmptying makes the scheduler behave, until the returned function
// is called, like an engine that emptied nothing: a directory is left as it
// stood, and no key carries the marker. Not for parallel tests: it swaps two
// package variables.
func engineWithoutEmptying(t *testing.T) (restore func()) {
	t.Helper()
	savedEmpty, savedIDs := emptyTaskOutput, cachekey.EmptiedOutputIDs
	emptyTaskOutput = func(string, string, ...string) error { return nil }
	cachekey.EmptiedOutputIDs = func(*ScheduledJob) []string { return nil }
	restore = func() { emptyTaskOutput, cachekey.EmptiedOutputIDs = savedEmpty, savedIDs }
	t.Cleanup(restore)
	return restore
}

// ---------------------------------------------------------------------------
// the rule, through the real scheduler
// ---------------------------------------------------------------------------

// The defect, end to end: a generator stops emitting a page and leaves the
// earlier file behind. The executing task must find no such file, the entry it
// stores must not hold it, and a later build from a deleted directory must not
// bring it back. What the task does not own stays where it is, under the same
// inode: the ceded subtree and the preserved one. A file another task wrote
// inside the owned region goes, as it does on a restore.
func TestExecutingTaskStartsFromAnEmptyOwnedDirectory(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-leftover-is-removed-and-never-stored")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-hit-of-a-present-output-and-an-adopted-result-empty-nothing")
	f := newEmptyingFixture(t, false)
	emptyings := recordEmptyings(t)

	f.setPages(t, "index", "about")
	first := f.executed(t, "two pages", f.run(t, SchedulerConfig{}))[0]
	assertFilesIn(t, "two pages", f.gen, "public/static/about.html", "public/static/index.html", "version.json")

	ceded := filepath.Join(f.gen, "migration-bundle", "0001.sql")
	preserved := filepath.Join(f.gen, "handwritten", "note.txt")
	writeFileAt(t, ceded, "ceded to another task\n")
	writeFileAt(t, preserved, "kept for the workspace\n")
	writeFileAt(t, filepath.Join(f.gen, "config-schema.json"), "{}\n")
	before := map[string]os.FileInfo{}
	for _, path := range []string{ceded, preserved, filepath.Dir(ceded), filepath.Dir(preserved), f.gen} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = info
	}

	// The about page is deleted: the generator writes index alone and removes
	// nothing.
	f.setPages(t, "index")
	second := f.executed(t, "one page", f.run(t, SchedulerConfig{}))[0]
	if second.CacheKey == "" || second.CacheKey == first.CacheKey {
		t.Fatalf("keys = %q then %q, want two distinct keys for two page sets", first.CacheKey, second.CacheKey)
	}
	assertFilesIn(t, "one page", f.gen,
		"handwritten/note.txt", "migration-bundle/0001.sql", "public/static/index.html", "version.json")
	for path, info := range before {
		now, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, now) {
			t.Errorf("%s was moved or replaced by the emptying (err=%v)", path, err)
		}
	}
	if got := readFileAt(t, ceded); got != "ceded to another task\n" {
		t.Errorf("ceded file = %q", got)
	}
	if got := readFileAt(t, preserved); got != "kept for the workspace\n" {
		t.Errorf("preserved file = %q", got)
	}
	if got, want := f.storedFiles(t, second.CacheKey), []string{"public/static/index.html", "version.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the stored entry holds %v, want %v: a leftover or an unowned path entered the cache", got, want)
	}

	// A build from a deleted directory is served from that entry and restores
	// what this page set produces, nothing else.
	if err := os.RemoveAll(f.gen); err != nil {
		t.Fatal(err)
	}
	f.hit(t, "restore", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "restore", f.gen, "public/static/index.html", "version.json")

	if got := invocationLog(t, f.logPath); len(got) != 2 {
		t.Fatalf("invocations = %v, want the two executions and no third", got)
	}
	// One emptying per execution, and none for the hit: a hit replaces the
	// directory through the restore alone.
	if got, want := emptyings(), []string{f.gen, f.gen}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emptied %v, want %v", got, want)
	}
}

// The scheduler writes the build stamp once per project and session, in the
// preparation of the first task that executes. A second task of that session
// that owns the directory holding the stamp must still find it when its
// subprocess starts, and its entry must hold it.
func TestSecondOwnerOfTheStampDirectoryStillFindsTheStamp(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	f := newEmptyingFixture(t, false, "build~generate", "test~generate")
	emptyings := recordEmptyings(t)
	f.setPages(t, "index")
	writeFileAt(t, filepath.Join(f.gen, "public", "static", "deleted.html"), "leftover\n")

	revision := stampCommitA()
	rows := f.executed(t, "two owners", f.run(t, SchedulerConfig{VersionInfo: revision}))

	// The tool logs the command name each job runs under.
	want := []string{"run:build:stamp-present", "run:test:stamp-present"}
	if got := invocationLog(t, f.logPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("invocations = %v, want %v: an owner started without the build stamp", got, want)
	}
	if got := emptyings(); len(got) != 2 {
		t.Fatalf("emptied %v, want one emptying per executing owner", got)
	}
	assertFilesIn(t, "two owners", f.gen, "public/static/index.html", "version.json")
	assertStampReports(t, "two owners", filepath.Join(f.ws.Root, emptyingProject), revision)
	for i, row := range rows {
		if got, want := f.storedFiles(t, row.CacheKey), []string{"public/static/index.html", "version.json"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s stored %v, want %v", f.planned[i].Key(), got, want)
		}
	}
}

// A task that adopts its leader's result executes nothing and captures the tree
// the leader wrote. It must empty nothing: emptying would delete that tree
// before the capture reads it.
func TestAdoptedResultEmptiesNothing(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-hit-of-a-present-output-and-an-adopted-result-empty-nothing")
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")
	gen := filepath.Join(ws.Root, "app", ".gen")
	writeExecutable(t, filepath.Join(ws.Root, "describe.sh"),
		"#!/bin/sh\nmkdir -p .gen\nprintf 'bundle\\n' > .gen/bundle.sql\n"+
			"printf 'run:%s\\n' \"$PUTNAMI_JOB_NAME\" >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\"}}'\n")
	for _, job := range planned {
		job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true}
		job.Extension.Tasks["probe-describe"] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{"bundle": dirDeclaration(extension.OutputRootProject, ".gen", false)},
		}}
	}
	writeFileAt(t, filepath.Join(gen, "leftover.sql"), "from an earlier run\n")
	emptyings := recordEmptyings(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	newCache := func() *store.CacheManager { return store.NewCacheManager(store.NewLocalStore(storeRoot)) }
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cold := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, newCache())
	if !cold.Success {
		t.Fatalf("shared run failed: %+v", cold.Results)
	}
	leader, follower := cold.Results[planned[0].Key()], cold.Results[planned[1].Key()]
	if leader.ReuseKind() != ReuseNone || follower.ReuseKind() != ReuseCoalesced {
		t.Fatalf("reuse = %q/%q, want a leader that executed and a follower that adopted", leader.ReuseKind(), follower.ReuseKind())
	}
	if got := invocationLog(t, logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want one", got)
	}
	if got, want := emptyings(), []string{gen}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emptied %v, want the leader's one emptying %v", got, want)
	}
	assertFilesIn(t, "shared", gen, "bundle.sql", "version.json")

	// The follower stored the leader's tree: its own entry restores it.
	if err := os.RemoveAll(gen); err != nil {
		t.Fatal(err)
	}
	warm := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, newCache())
	for _, job := range planned {
		if row := warm.Results[job.Key()]; row == nil || row.ReuseKind() != ReuseLocalCache {
			t.Fatalf("warm %s = %+v, want a restore from its own entry", job.Key(), row)
		}
	}
	assertFilesIn(t, "warm", gen, "bundle.sql", "version.json")
	if got := emptyings(); len(got) != 1 {
		t.Fatalf("a hit emptied a directory: %v", got)
	}
}

// A directory that cannot be emptied may still hold an earlier run's files. The
// task fails before its subprocess starts, with an error that names the output,
// and nothing is stored. No retry runs the tool against that directory either.
func TestFailureToEmptyFailsTheTaskBeforeItRuns(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-failure-to-empty-fails-the-task-before-it-runs")
	f := newEmptyingFixture(t, false)
	f.setPages(t, "index")
	leftover := filepath.Join(f.gen, "public", "static", "deleted.html")
	writeFileAt(t, leftover, "leftover\n")

	saved := emptyTaskOutput
	emptyTaskOutput = func(string, string, ...string) error { return errors.New("device busy") }
	t.Cleanup(func() { emptyTaskOutput = saved })

	result := f.run(t, SchedulerConfig{Retry: 2})
	row := f.rows(t, "cannot empty", result)[0]
	if result.Success || row.Status != "failed" || row.Error == nil {
		t.Fatalf("row = %+v (success=%v), want a failed task", row, result.Success)
	}
	for _, want := range []string{"empty declared outputs", `declared output "gen"`, "device busy"} {
		if !strings.Contains(row.Error.Message, want) {
			t.Errorf("error %q does not name %q", row.Error.Message, want)
		}
	}
	if got := invocationLog(t, f.logPath); len(got) != 0 {
		t.Fatalf("invocations = %v, want none: the tool ran against a directory that was not emptied", got)
	}
	if got := countTaskEntryDescriptors(t, f.storeRoot); got != 0 {
		t.Fatalf("entries stored = %d, want none", got)
	}
	if got := readFileAt(t, leftover); got != "leftover\n" {
		t.Fatalf("leftover = %q", got)
	}

	// Once the directory can be emptied, the same inputs execute and store.
	emptyTaskOutput = saved
	recovered := f.executed(t, "recovered", f.run(t, SchedulerConfig{}))[0]
	assertFilesIn(t, "recovered", f.gen, "public/static/index.html", "version.json")
	if got, want := f.storedFiles(t, recovered.CacheKey), []string{"public/static/index.html", "version.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}
}

// A retry is another execution: what the failed attempt wrote is not the output
// of the attempt that follows, on disk or in the entry.
func TestRetryStartsFromAnEmptyOwnedDirectory(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-retry-starts-from-an-empty-directory")
	f := newEmptyingFixture(t, true)
	emptyings := recordEmptyings(t)
	f.setPages(t, "index")

	row := f.executed(t, "retried", f.run(t, SchedulerConfig{Retry: 1}))[0]
	if got := invocationLog(t, f.logPath); len(got) != 2 {
		t.Fatalf("invocations = %v, want the failed attempt and its retry", got)
	}
	if got := emptyings(); len(got) != 2 {
		t.Fatalf("emptied %v, want one emptying per attempt", got)
	}
	assertFilesIn(t, "retried", f.gen, "public/static/index.html", "version.json")
	if got, want := f.storedFiles(t, row.CacheKey), []string{"public/static/index.html", "version.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v: the failed attempt's file entered the cache", got, want)
	}
}

// A retry that cannot be given an empty directory is not attempted. The task
// keeps the failure of the attempt that ran, and nothing is stored.
func TestRetryThatCannotEmptyIsNotAttempted(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-retry-starts-from-an-empty-directory")
	f := newEmptyingFixture(t, true)
	f.setPages(t, "index")

	saved := emptyTaskOutput
	calls := 0
	emptyTaskOutput = func(dest, stagingRoot string, keep ...string) error {
		calls++
		if calls > 1 {
			return errors.New("device busy")
		}
		return saved(dest, stagingRoot, keep...)
	}
	t.Cleanup(func() { emptyTaskOutput = saved })

	result := f.run(t, SchedulerConfig{Retry: 3})
	row := f.rows(t, "retry refused", result)[0]
	if result.Success || row.Status != "failed" {
		t.Fatalf("row = %+v, want the first attempt's failure", row)
	}
	if row.Error != nil && strings.Contains(row.Error.Message, "empty declared outputs") {
		t.Fatalf("the row lost the attempt's own failure: %q", row.Error.Message)
	}
	if got := invocationLog(t, f.logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want the first attempt alone", got)
	}
	if calls != 2 {
		t.Fatalf("emptyings asked = %d, want the first attempt's and one refused retry", calls)
	}
	if got := countTaskEntryDescriptors(t, f.storeRoot); got != 0 {
		t.Fatalf("entries stored = %d, want none", got)
	}
}

// An entry stored by an engine that emptied nothing can hold a leftover under
// the key of sources that no longer produce it. The first half reproduces that
// engine and the entry it leaves. The second half is this engine reading the
// same store with the same sources: it must not be served that entry.
func TestEntryStoredWithoutEmptyingIsNeverServed(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-entry-stored-without-emptying-is-never-served")
	f := newEmptyingFixture(t, false)

	restore := engineWithoutEmptying(t)
	f.setPages(t, "index", "about")
	f.executed(t, "old engine, two pages", f.run(t, SchedulerConfig{}))
	f.setPages(t, "index")
	poisoned := f.executed(t, "old engine, one page", f.run(t, SchedulerConfig{}))[0]
	leftover := []string{"public/static/about.html", "public/static/index.html", "version.json"}
	if got := f.storedFiles(t, poisoned.CacheKey); !reflect.DeepEqual(got, leftover) {
		t.Fatalf("the old engine stored %v, want the leftover in the entry (%v): the fixture no longer reproduces the defect", got, leftover)
	}
	if err := os.RemoveAll(f.gen); err != nil {
		t.Fatal(err)
	}
	f.hit(t, "old engine, restore", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "old engine, restore", f.gen, leftover...)

	restore()
	if err := os.RemoveAll(f.gen); err != nil {
		t.Fatal(err)
	}
	clean := f.executed(t, "this engine", f.run(t, SchedulerConfig{}))[0]
	if clean.CacheKey == poisoned.CacheKey {
		t.Fatalf("this engine computed the old engine's key %s for a task it empties", clean.CacheKey)
	}
	assertFilesIn(t, "this engine", f.gen, "public/static/index.html", "version.json")
	if got, want := f.storedFiles(t, clean.CacheKey), []string{"public/static/index.html", "version.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}

	if err := os.RemoveAll(f.gen); err != nil {
		t.Fatal(err)
	}
	f.hit(t, "this engine, restore", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "this engine, restore", f.gen, "public/static/index.html", "version.json")
}

// An optionalEmpty directory under the project root that does not declare
// emptied can hold a file no task wrote, such as a committed configuration the
// task reads when it writes nothing there. A run that writes nothing is
// recorded empty, and a restore of that record leaves the path as it stands.
// An execution that writes nothing must leave it too, so the scheduler never
// empties such an output.
func TestOptionalOutputKeepsAFileThroughAnExecutionThatWritesNothing(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-project-or-workspace-output-is-left-as-it-stands")
	f := newEmptyingFixture(t, false)
	f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"clientgen": dirDeclaration(extension.OutputRootProject, ".gen/clientgen", true),
		},
	}}
	emptyings := recordEmptyings(t)
	committed := filepath.Join(f.gen, "clientgen", "config.json")

	// With no page listed, the tool writes nothing under .gen/clientgen, and
	// the entry records the output empty.
	first := f.executed(t, "nothing written", f.run(t, SchedulerConfig{}))[0]
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(first.CacheKey)
	if err != nil || entry == nil || len(entry.Outputs) != 1 || entry.Outputs[0].State != store.TaskOutputEmpty {
		t.Fatalf("entry = %+v (err=%v), want the one output recorded empty", entry, err)
	}

	writeFileAt(t, committed, "{\"targets\":[\"go\"]}\n")
	before, err := os.Lstat(committed)
	if err != nil {
		t.Fatal(err)
	}
	assertKept := func(leg string) {
		t.Helper()
		now, err := os.Lstat(committed)
		if err != nil || !os.SameFile(before, now) {
			t.Fatalf("%s: the committed file was removed or replaced (err=%v)", leg, err)
		}
		if got := readFileAt(t, committed); got != "{\"targets\":[\"go\"]}\n" {
			t.Fatalf("%s: committed file = %q", leg, got)
		}
	}

	// The restore of the empty record leaves the file.
	f.hit(t, "restore of the empty record", f.run(t, SchedulerConfig{}))
	assertKept("restore of the empty record")

	// A blank line moves the key and names no page: the task executes again
	// and writes nothing under .gen/clientgen. It leaves the file, as the
	// restore did.
	f.setPages(t, "")
	f.executed(t, "execution that writes nothing", f.run(t, SchedulerConfig{}))
	assertKept("execution that writes nothing")

	if got := emptyings(); len(got) != 0 {
		t.Fatalf("emptied %v, want nothing: the task owns no emptied directory", got)
	}
}

// An optionalEmpty project directory that declares emptied holds only what
// its task writes (ADR 0068). The scheduler empties it before the task
// executes and when it restores an entry that records it empty, and keeps the
// ceded and preserved subpaths under the same inode, so a contract an earlier
// run left there survives neither an execution that writes nothing nor the
// restore of its record.
func TestEmptiedOptionalOutputIsEmptiedOnExecutionAndOnAnEmptyRestore(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-emptied-optional-project-or-workspace-output-is-emptied")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	f := newEmptyingFixture(t, false)
	f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"clientgen": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen/clientgen",
				OptionalEmpty: true, Emptied: true, Excludes: []string{".gen/clientgen/ceded"}, Preserves: []string{"kept"},
			},
		},
	}}
	emptyings := recordEmptyings(t)
	clientgen := filepath.Join(f.gen, "clientgen")
	stale := filepath.Join(clientgen, "config.json")

	// An earlier run left a contract; this one writes nothing under
	// .gen/clientgen. The execution starts without the contract, and the entry
	// records the output empty.
	writeFileAt(t, stale, "{\"targets\":[\"go\"]}\n")
	first := f.executed(t, "execution over a stale contract", f.run(t, SchedulerConfig{}))[0]
	assertFilesIn(t, "execution over a stale contract", clientgen)
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(first.CacheKey)
	if err != nil || entry == nil {
		t.Fatalf("no entry under %s (err=%v)", first.CacheKey, err)
	}
	if recorded, ok := entry.Output("clientgen"); !ok || recorded.State != store.TaskOutputEmpty {
		t.Fatalf("clientgen recorded as %+v, want the output recorded empty", recorded)
	}

	// The restore of that record over a stale contract leaves the tree the
	// execution left: the ceded and preserved subpaths, and nothing else.
	unowned := writeUnowned(t, clientgen)
	writeFileAt(t, stale, "{\"targets\":[\"go\"]}\n")
	f.hit(t, "restore of the empty record", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "restore of the empty record", clientgen, "ceded/other", "kept/note.txt")
	assertUnmoved(t, "restore of the empty record", unowned)

	if got := invocationLog(t, f.logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want the one execution", got)
	}
	if got, want := emptyings(), []string{clientgen, clientgen}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emptied %v, want the execution's emptying and the restore's", got)
	}
}

// ---------------------------------------------------------------------------
// an optional command-output directory, through the real scheduler
// ---------------------------------------------------------------------------

// newBinFixture gives the fixture project the shape of the Go cross-compile:
// its task owns the optionalEmpty command-output directory bin, cedes
// bin/ceded to another task and preserves bin/kept for the workspace. Its tool
// writes one binary per line of pages.txt, as bin/<line>, and never removes
// one. It returns the fixture and the absolute path of bin.
func newBinFixture(t *testing.T) (*emptyingFixture, string) {
	t.Helper()
	f := newEmptyingFixture(t, false)
	f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"bin": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "bin",
				OptionalEmpty: true, Excludes: []string{"bin/ceded"}, Preserves: []string{"kept"},
			},
		},
	}}
	writeExecutable(t, filepath.Join(f.ws.Root, "generate.sh"), "#!/bin/sh\n"+
		"printf 'run:%s\\n' \"$PUTNAMI_JOB_NAME\" >> "+shellQuote(f.logPath)+"\n"+
		"mkdir -p \"$PUTNAMI_OUTPUT_PATH/bin\"\n"+
		"while IFS= read -r name; do\n"+
		"  if [ -n \"$name\" ]; then printf 'binary %s\\n' \"$name\" > \"$PUTNAMI_OUTPUT_PATH/bin/$name\"; fi\n"+
		"done < pages.txt\n"+
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n")
	return f, filepath.Join(f.ws.Root, ".putnami", "out", emptyingProject, "build", "bin")
}

// writeUnowned writes a file at the ceded and at the preserved subpath of bin
// and returns the identity of each path the emptying must leave in place:
// both files, the directories that hold them, and bin itself.
func writeUnowned(t *testing.T, bin string) map[string]os.FileInfo {
	t.Helper()
	ceded := filepath.Join(bin, "ceded", "other")
	preserved := filepath.Join(bin, "kept", "note.txt")
	writeFileAt(t, ceded, "ceded to another task\n")
	writeFileAt(t, preserved, "kept for the workspace\n")
	unowned := map[string]os.FileInfo{}
	for _, path := range []string{ceded, preserved, filepath.Dir(ceded), filepath.Dir(preserved), bin} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		unowned[path] = info
	}
	return unowned
}

func assertUnmoved(t *testing.T, leg string, unowned map[string]os.FileInfo) {
	t.Helper()
	for path, info := range unowned {
		now, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, now) {
			t.Errorf("%s: %s was moved or replaced (err=%v)", leg, path, err)
		}
	}
}

// The defect, end to end, on the shape of the Go cross-compile: a project
// drops a platform, and the binary built for it stays in bin. The execution
// that follows must find no such binary, the entry it stores must not hold it,
// and a later build from a deleted directory must not bring it back. The ceded
// and preserved subpaths stay, under the same inode. A hit that restores a
// present output replaces the directory through the restore alone.
func TestDroppedPlatformLeavesNoBinaryBehind(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-command-output-directory-is-emptied")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-leftover-is-removed-and-never-stored")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-hit-of-a-present-output-and-an-adopted-result-empty-nothing")
	f, bin := newBinFixture(t)
	emptyings := recordEmptyings(t)

	f.setPages(t, "a", "b")
	first := f.executed(t, "two platforms", f.run(t, SchedulerConfig{}))[0]
	assertFilesIn(t, "two platforms", bin, "a", "b")
	if got, want := f.storedOutput(t, first.CacheKey, "bin"), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the first entry holds %v, want %v", got, want)
	}
	unowned := writeUnowned(t, bin)

	// The project drops platform b: the tool writes a alone and removes nothing.
	f.setPages(t, "a")
	second := f.executed(t, "one platform", f.run(t, SchedulerConfig{}))[0]
	if second.CacheKey == "" || second.CacheKey == first.CacheKey {
		t.Fatalf("keys = %q then %q, want two distinct keys for two platform sets", first.CacheKey, second.CacheKey)
	}
	assertFilesIn(t, "one platform", bin, "a", "ceded/other", "kept/note.txt")
	assertUnmoved(t, "one platform", unowned)
	if got, want := f.storedOutput(t, second.CacheKey, "bin"), []string{"a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the stored entry holds %v, want %v: the dropped platform's binary entered the cache", got, want)
	}

	// A hit over a stale binary restores the present output and prunes it.
	writeFileAt(t, filepath.Join(bin, "b"), "stale\n")
	f.hit(t, "restore over a stale binary", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "restore over a stale binary", bin, "a", "ceded/other", "kept/note.txt")

	// A build from a deleted directory restores the one binary.
	if err := os.RemoveAll(bin); err != nil {
		t.Fatal(err)
	}
	f.hit(t, "restore", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "restore", bin, "a")

	if got := invocationLog(t, f.logPath); len(got) != 2 {
		t.Fatalf("invocations = %v, want the two executions and no third", got)
	}
	if got, want := emptyings(), []string{bin, bin}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emptied %v, want one emptying per execution and none for a hit of a present output", got)
	}
}

// A run that builds nothing records bin empty. A hit on that entry must leave
// bin as that run left it: it empties what the task owns there, keeps the
// ceded and preserved subpaths under the same inode, and runs no tool.
// Leaving the destination of the empty record as it stood would serve a stale
// binary beside a green status.
func TestRestoreOfAnEmptyCommandOutputRecordEmptiesIt(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-restore-of-an-empty-command-output-record-empties-it")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	f, bin := newBinFixture(t)
	emptyings := recordEmptyings(t)

	first := f.executed(t, "nothing built", f.run(t, SchedulerConfig{}))[0]
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(first.CacheKey)
	if err != nil || entry == nil {
		t.Fatalf("no entry under %s (err=%v)", first.CacheKey, err)
	}
	if recorded, ok := entry.Output("bin"); !ok || recorded.State != store.TaskOutputEmpty {
		t.Fatalf("bin recorded as %+v, want the output recorded empty", recorded)
	}

	unowned := writeUnowned(t, bin)
	writeFileAt(t, filepath.Join(bin, "a"), "stale\n")
	writeFileAt(t, filepath.Join(bin, "linux-amd64", "a"), "stale\n")

	f.hit(t, "restore of the empty record", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "restore of the empty record", bin, "ceded/other", "kept/note.txt")
	assertUnmoved(t, "restore of the empty record", unowned)

	if got := invocationLog(t, f.logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want the one execution", got)
	}
	if got, want := emptyings(), []string{bin, bin}; !reflect.DeepEqual(got, want) {
		t.Fatalf("emptied %v, want the execution's emptying and the restore's", got)
	}

	// The shape of the Go package-archives task: the empty directory sits beside
	// a required file. The entry records the directory empty and the file
	// present. The restore empties the directory and replaces the file.
	t.Run("an empty directory beside a required file", func(t *testing.T) {
		f, archives := newBinFixture(t)
		f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"archives": {
					Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "bin",
					OptionalEmpty: true,
				},
				"metadata": fileDeclaration(extension.OutputRootCommandOutput, "metadata.json", false),
			},
		}}
		writeExecutable(t, filepath.Join(f.ws.Root, "generate.sh"), "#!/bin/sh\n"+
			"printf 'run:%s\\n' \"$PUTNAMI_JOB_NAME\" >> "+shellQuote(f.logPath)+"\n"+
			"mkdir -p \"$PUTNAMI_OUTPUT_PATH/bin\"\n"+
			"printf '{\"archives\":0}\\n' > \"$PUTNAMI_OUTPUT_PATH/metadata.json\"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n")
		metadata := filepath.Join(filepath.Dir(archives), "metadata.json")

		first := f.executed(t, "nothing archived", f.run(t, SchedulerConfig{}))[0]
		entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(first.CacheKey)
		if err != nil || entry == nil {
			t.Fatalf("no entry under %s (err=%v)", first.CacheKey, err)
		}
		if recorded, ok := entry.Output("archives"); !ok || recorded.State != store.TaskOutputEmpty {
			t.Fatalf("archives recorded as %+v, want the output recorded empty", recorded)
		}
		if recorded, ok := entry.Output("metadata"); !ok || recorded.State == store.TaskOutputEmpty {
			t.Fatalf("metadata recorded as %+v, want the file recorded present", recorded)
		}

		writeFileAt(t, filepath.Join(archives, "linux-amd64", "app.tar.gz"), "stale\n")
		writeFileAt(t, metadata, "stale bytes\n")

		f.hit(t, "restore beside a file", f.run(t, SchedulerConfig{}))
		assertFilesIn(t, "restore beside a file", archives)
		if got := readFileAt(t, metadata); got != "{\"archives\":0}\n" {
			t.Fatalf("metadata.json = %q, want the entry's bytes", got)
		}
		if got := invocationLog(t, f.logPath); len(got) != 1 {
			t.Fatalf("invocations = %v, want the one execution", got)
		}
	})
}

// A restore that cannot empty an output it must empty can leave a stale binary
// beside a green status. That is a restore failure like any other, so the hit
// is not served. What follows is what follows any failed restore. A key of
// unknown cost is worth coalescing: it finds its entry published and restores
// it once more, then executes when that restore fails too. Either way the run
// ends with bin emptied, or with the task failed before its tool runs when
// bin cannot be emptied at all.
func TestFailureToEmptyOnRestoreExecutesTheTask(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-failure-to-empty-on-restore-executes-the-task")

	// prepare stores an entry that records bin empty, leaves a stale binary in
	// bin, and makes the emptying fail on each call fails names. It drops the
	// run history so the next run sees the key's cost as unknown.
	prepare := func(t *testing.T, fails func(call int) bool) (*emptyingFixture, string, *int) {
		t.Helper()
		f, bin := newBinFixture(t)
		f.executed(t, "nothing built", f.run(t, SchedulerConfig{}))
		writeFileAt(t, filepath.Join(bin, "a"), "stale\n")
		if err := os.Remove(taskStatsPath(f.ws.Root)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		saved := emptyTaskOutput
		calls := 0
		emptyTaskOutput = func(dest, stagingRoot string, keep ...string) error {
			calls++
			if fails(calls) {
				return errors.New("device busy")
			}
			return saved(dest, stagingRoot, keep...)
		}
		t.Cleanup(func() { emptyTaskOutput = saved })
		return f, bin, &calls
	}

	t.Run("a second restore serves the entry", func(t *testing.T) {
		f, bin, calls := prepare(t, func(call int) bool { return call == 1 })
		row := f.executed(t, "first restore cannot empty", f.run(t, SchedulerConfig{}))[0]
		if row.ReuseKind() != ReuseCoalesced {
			t.Fatalf("reuse = %q, want the published entry restored by the claim-or-wait path", row.ReuseKind())
		}
		if *calls != 2 {
			t.Fatalf("emptyings asked = %d, want the failed restore's and the second restore's", *calls)
		}
		if got := invocationLog(t, f.logPath); len(got) != 1 {
			t.Fatalf("invocations = %v, want the first execution alone", got)
		}
		assertFilesIn(t, "first restore cannot empty", bin)
	})

	t.Run("a restore that keeps failing executes the task", func(t *testing.T) {
		f, bin, calls := prepare(t, func(call int) bool { return call <= 2 })
		row := f.executed(t, "no restore can empty", f.run(t, SchedulerConfig{}))[0]
		if row.ReuseKind() != ReuseNone {
			t.Fatalf("reuse = %q, want an execution", row.ReuseKind())
		}
		if *calls != 3 {
			t.Fatalf("emptyings asked = %d, want two failed restores and the execution's", *calls)
		}
		if got := invocationLog(t, f.logPath); len(got) != 2 {
			t.Fatalf("invocations = %v, want the first execution and the one that replaced the hit", got)
		}
		assertFilesIn(t, "no restore can empty", bin)
	})

	t.Run("an output that cannot be emptied fails the task", func(t *testing.T) {
		f, bin, _ := prepare(t, func(int) bool { return true })
		result := f.run(t, SchedulerConfig{})
		row := f.rows(t, "cannot empty", result)[0]
		if result.Success || row.Status != "failed" || row.CacheHit || row.Error == nil ||
			!strings.Contains(row.Error.Message, "empty declared outputs") {
			t.Fatalf("row = %+v (success=%v), want the task failed before it runs", row, result.Success)
		}
		if got := invocationLog(t, f.logPath); len(got) != 1 {
			t.Fatalf("invocations = %v, want the first execution alone", got)
		}
		assertFilesIn(t, "cannot empty", bin, "a")
	})
}

// An entry an engine that emptied nothing stored for an optional
// command-output directory can hold the binary of a platform the project
// dropped. This engine computes another key for the same sources, so it never
// serves that entry.
func TestBinaryEntryStoredWithoutEmptyingIsNeverServed(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-entry-stored-without-emptying-is-never-served")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-command-output-directory-is-emptied")
	f, bin := newBinFixture(t)

	restore := engineWithoutEmptying(t)
	f.setPages(t, "a", "b")
	f.executed(t, "old engine, two platforms", f.run(t, SchedulerConfig{}))
	f.setPages(t, "a")
	poisoned := f.executed(t, "old engine, one platform", f.run(t, SchedulerConfig{}))[0]
	if got, want := f.storedOutput(t, poisoned.CacheKey, "bin"), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the old engine stored %v, want the leftover in the entry (%v): the fixture no longer reproduces the defect", got, want)
	}
	restore()

	clean := f.executed(t, "this engine", f.run(t, SchedulerConfig{}))[0]
	if clean.CacheKey == poisoned.CacheKey {
		t.Fatalf("this engine computed the old engine's key %s for a task it empties", clean.CacheKey)
	}
	assertFilesIn(t, "this engine", bin, "a")
	if got, want := f.storedOutput(t, clean.CacheKey, "bin"), []string{"a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// which outputs are emptied, and what the key says about them
// ---------------------------------------------------------------------------

// emptyingShapesJob declares one output of every shape: a required directory
// at each of the three durable roots and at the default root, an optional
// directory under the command-output root, and an optional project and
// workspace directory that declare emptied, which are emptied, and the shapes
// that are not.
func emptyingShapesJob() *ScheduledJob {
	return declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"gen": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen",
				Excludes: []string{".gen/migration-bundle"}, Preserves: []string{"handwritten"},
			},
			"shared":  dirDeclaration(extension.OutputRootWorkspace, "dist/shared", false),
			"default": {Kind: extension.OutputKindDirectory, Path: "generated"},
			"archives": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "archives",
				OptionalEmpty: true, Excludes: []string{"archives/ceded"}, Preserves: []string{"kept"},
			},
			"optional":       dirDeclaration(extension.OutputRootProject, "described", true),
			"optionalShared": dirDeclaration(extension.OutputRootWorkspace, "dist/optional", true),
			"emptiedOptional": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "contract",
				OptionalEmpty: true, Emptied: true, Excludes: []string{"contract/ceded"}, Preserves: []string{"kept"},
			},
			"emptiedShared": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootWorkspace, Path: "dist/contracts",
				OptionalEmpty: true, Emptied: true,
			},
			"tool":    fileDeclaration(extension.OutputRootCommandOutput, "bin/tool", true),
			"client":  {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
			"scratch": {Kind: extension.OutputKindDirectory, Scope: extensionproto.OutputScopeInvocation, Path: "scratch", OptionalEmpty: true},
			"socket":  {Kind: extensionproto.OutputKindRuntimeFile, Scope: extensionproto.OutputScopeInvocation, Path: "socket", OptionalEmpty: true},
			"escape":  {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "../outside", OptionalEmpty: true},
			"root":    {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".", OptionalEmpty: true},
			"unknown": {Kind: extension.OutputKindDirectory, Root: "elsewhere", Path: "out", OptionalEmpty: true},
		},
	})
}

// The build stamp is kept in the one output that holds it: a project-rooted
// directory the stamp sits strictly inside, under any spelling of its name.
func TestVersionStampWithin(t *testing.T) {
	for _, tc := range []struct {
		root, rel string
		want      string
		held      bool
	}{
		{extension.OutputRootProject, ".gen", "version.json", true},
		{extension.OutputRootProject, ".GEN", "version.json", true},
		{extension.OutputRootProject, ".gen/version.json", "", false},
		{extension.OutputRootProject, ".gen/public", "", false},
		{extension.OutputRootProject, ".ge", "", false},
		{extension.OutputRootProject, ".generated", "", false},
		{extension.OutputRootProject, "dist", "", false},
		{extension.OutputRootWorkspace, ".gen", "", false},
		{extension.OutputRootCommandOutput, ".gen", "", false},
	} {
		got, held := versionStampWithin(tc.root, tc.rel)
		if got != tc.want || held != tc.held {
			t.Errorf("versionStampWithin(%q, %q) = %q, %v; want %q, %v", tc.root, tc.rel, got, held, tc.want, tc.held)
		}
	}
}

// Each emptied output loses what its task owns and keeps the rest, at each of
// the three roots. Nothing beside an output is touched: the project root, the
// workspace root and the command-output directory are never emptied. The
// outputs that are not emptied keep what they hold: a committed file in an
// optionalEmpty project or workspace directory that does not declare emptied,
// and an earlier run's files in the other shapes, which for a pathFrom output
// is the documented limit.
func TestEmptyOwnedOutputsPerRoot(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-path-from-output-is-left-as-it-stands")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-project-or-workspace-output-is-left-as-it-stands")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-command-output-directory-is-emptied")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-emptied-optional-project-or-workspace-output-is-emptied")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	job := emptyingShapesJob()
	f := newCaptureFixture(t, job)
	project := filepath.Join(f.ws.Root, captureTestProject)
	command := f.commandDir("build")

	owned := []string{
		filepath.Join(command, "lib", "stale.js"),
		filepath.Join(command, "lib", "nested", "stale.js"),
		filepath.Join(command, "archives", "linux-amd64", "app.tar.gz"),
		filepath.Join(command, "archives", "channel.json"),
		filepath.Join(project, ".gen", "public", "static", "deleted.html"),
		filepath.Join(project, ".gen", "config-schema.json"),
		filepath.Join(project, "generated", "stale.ts"),
		filepath.Join(f.ws.Root, "dist", "shared", "stale.txt"),
		filepath.Join(project, "contract", "openapi.json"),
		filepath.Join(project, "contract", "nested", "config.json"),
		filepath.Join(f.ws.Root, "dist", "contracts", "stale.json"),
	}
	kept := []string{
		// Ceded, preserved, and the scheduler's stamp.
		filepath.Join(project, ".gen", "migration-bundle", "0001.sql"),
		filepath.Join(project, ".gen", "handwritten", "note.txt"),
		filepath.Join(project, ".gen", "version.json"),
		filepath.Join(command, "archives", "ceded", "other.tar.gz"),
		filepath.Join(command, "archives", "kept", "note.txt"),
		filepath.Join(project, "contract", "ceded", "other.json"),
		filepath.Join(project, "contract", "kept", "note.txt"),
		// Beside the outputs, under each root.
		filepath.Join(command, "other", "sibling.js"),
		filepath.Join(project, "src", "main.ts"),
		filepath.Join(project, "putnami.json"),
		filepath.Join(f.ws.Root, "dist", "other", "sibling.txt"),
		filepath.Join(f.ws.Root, "README.md"),
		// Outputs that are not emptied: an optionalEmpty project or workspace
		// directory, a file, a pathFrom directory, and the paths the
		// invocation-scoped, escaping and unknown-root declarations would name
		// under the project.
		filepath.Join(project, "described", "config.json"),
		filepath.Join(f.ws.Root, "dist", "optional", "config.json"),
		filepath.Join(command, "bin", "tool"),
		filepath.Join(project, "src", "clients", "removed-endpoint.ts"),
		filepath.Join(project, "scratch", "kept.txt"),
		filepath.Join(project, "out", "kept.txt"),
		filepath.Join(f.ws.Root, "outside", "kept.txt"),
	}
	for _, path := range append(append([]string{}, owned...), kept...) {
		writeFileAt(t, path, "from an earlier run\n")
	}

	if err := f.sched.emptyOwnedOutputs(job); err != nil {
		t.Fatalf("emptyOwnedOutputs: %v", err)
	}

	for _, path := range owned {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("owned %s is still there (err=%v)", path, err)
		}
	}
	for _, path := range kept {
		if got := readFileAt(t, path); got != "from an earlier run\n" {
			t.Errorf("%s = %q, want it untouched", path, got)
		}
	}
	// An emptied output directory stays, empty.
	for _, dir := range []string{filepath.Join(command, "lib"), filepath.Join(project, "generated"), filepath.Join(f.ws.Root, "dist", "shared")} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s: entries=%v err=%v, want an empty directory", dir, entries, err)
		}
	}
	// Nothing is left where the removed entries were staged.
	if got := filesIn(t, store.ResolveScratchRoot(f.ws.Root)); len(got) != 0 {
		t.Errorf("the scratch root kept %v", got)
	}

	// Emptying again changes nothing, and an output that does not exist yet is
	// not created.
	if err := os.RemoveAll(filepath.Join(project, "generated")); err != nil {
		t.Fatal(err)
	}
	if err := f.sched.emptyOwnedOutputs(job); err != nil {
		t.Fatalf("second emptyOwnedOutputs: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, "generated")); !os.IsNotExist(err) {
		t.Errorf("an absent output was created (err=%v)", err)
	}
}

// Without a workspace root an output path would resolve against the working
// directory. The emptying refuses instead of removing anything there.
func TestEmptyOwnedOutputsRefusesWithoutAWorkspaceRoot(t *testing.T) {
	job := emptyingShapesJob()
	if err := (&Scheduler{}).emptyOwnedOutputs(job); err == nil || !strings.Contains(err.Error(), "no workspace root") {
		t.Fatalf("emptyOwnedOutputs without a workspace = %v, want a refusal", err)
	}
	// A task with nothing to empty needs no root.
	if err := (&Scheduler{}).emptyOwnedOutputs(declaredJob("lint", "lint", &extension.TaskDeclaration{})); err != nil {
		t.Fatalf("emptyOwnedOutputs of a task with no output = %v", err)
	}
}

// The marker moves the key of a task that owns an emptied directory, and of no
// other task: a task with no declared output, an optionalEmpty project or
// workspace directory that does not declare emptied, a file output or a
// pathFrom output keeps the address an engine that emptied nothing computed
// for it. An optionalEmpty command-output directory and an optionalEmpty
// project directory that declares emptied are emptied, so their tasks move. The selection key, which a
// release-set member records, moves exactly when the execution key does. A
// member republishes when its own task owns an emptied directory or when one
// of its upstream keys moved.
func TestEmptiedOutputsMoveOnlyTheKeysOfTasksThatOwnOne(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-entry-stored-without-emptying-is-never-served")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-project-or-workspace-output-is-left-as-it-stands")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-command-output-directory-is-emptied")
	for name, tc := range map[string]struct {
		outputs map[string]extension.DeclaredOutput
		moves   bool
	}{
		"required directory": {
			outputs: map[string]extension.DeclaredOutput{"gen": dirDeclaration(extension.OutputRootProject, ".gen", false)},
			moves:   true,
		},
		"required and optional directories": {
			outputs: map[string]extension.DeclaredOutput{
				"gen":       dirDeclaration(extension.OutputRootProject, ".gen", false),
				"clientgen": dirDeclaration(extension.OutputRootProject, "clientgen", true),
			},
			moves: true,
		},
		"optional directory": {
			outputs: map[string]extension.DeclaredOutput{"clientgen": dirDeclaration(extension.OutputRootProject, ".gen/clientgen", true)},
		},
		"optional workspace directory": {
			outputs: map[string]extension.DeclaredOutput{"shared": dirDeclaration(extension.OutputRootWorkspace, "dist/shared", true)},
		},
		"emptied optional project directory": {
			outputs: map[string]extension.DeclaredOutput{"clientgen": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen/clientgen",
				OptionalEmpty: true, Emptied: true,
			}},
			moves: true,
		},
		"optional command-output directory": {
			outputs: map[string]extension.DeclaredOutput{"bin": dirDeclaration(extension.OutputRootCommandOutput, "bin", true)},
			moves:   true,
		},
		"no output": {outputs: nil},
		"file": {
			outputs: map[string]extension.DeclaredOutput{"tool": fileDeclaration(extension.OutputRootCommandOutput, "bin/tool", false)},
		},
		"pathFrom": {
			outputs: map[string]extension.DeclaredOutput{"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{Outputs: tc.outputs})
			job.JobDef.FilePatterns = []string{"src.txt"}
			f := newCaptureFixture(t, job)
			writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "src.txt"), "stable\n")
			keys := func() (execution, selection string) {
				t.Helper()
				var err error
				if execution, err = cachekey.ComputeJobCacheHashWith(f.ws, job, nil, nil, f.cache, nil, false); err != nil {
					t.Fatalf("compute execution key: %v", err)
				}
				if selection, err = cachekey.ComputeJobCacheHashWith(f.ws, job, nil, nil, f.cache, nil, true); err != nil {
					t.Fatalf("compute selection key: %v", err)
				}
				return execution, selection
			}

			current, currentSelection := keys()
			restore := engineWithoutEmptying(t)
			earlier, earlierSelection := keys()
			restore()

			if moved := current != earlier; moved != tc.moves {
				t.Fatalf("key moved = %v, want %v (this engine %s, an engine that emptied nothing %s)",
					moved, tc.moves, current, earlier)
			}
			if moved := currentSelection != earlierSelection; moved != tc.moves {
				t.Fatalf("selection key moved = %v, want %v (this engine %s, an engine that emptied nothing %s)",
					moved, tc.moves, currentSelection, earlierSelection)
			}
		})
	}
}
