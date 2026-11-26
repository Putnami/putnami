package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// emptiedFileFixture is the emptying fixture whose task declares one
// optionalEmpty project file that declares emptied: the page index.html,
// which the tool writes only when pages.txt names it.
func emptiedFileFixture(t *testing.T) (*emptyingFixture, string) {
	t.Helper()
	f := newEmptyingFixture(t, false)
	f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"index": {
				Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: ".gen/public/static/index.html",
				OptionalEmpty: true, Emptied: true,
			},
		},
	}}
	return f, filepath.Join(f.gen, "public", "static", "index.html")
}

// An optionalEmpty file that declares emptied is the outcome of its task's
// last execution or restore, its absence included (ADR 0107). The scheduler
// removes it before the task executes and when it restores an entry that
// records it absent, so a file an earlier run left survives neither an
// execution that writes none nor the restore of its record. A restore of a
// present record writes the file back.
func TestEmptiedFileOutputIsRemovedOnExecutionAndOnAnAbsentRestore(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-emptied-file-output-is-removed-on-execution-and-on-an-absent-restore")
	f, index := emptiedFileFixture(t)

	// A run that writes the page records it present.
	f.setPages(t, "index")
	f.executed(t, "execution that writes the page", f.run(t, SchedulerConfig{}))
	if got := readFileAt(t, index); got != "<h1>index</h1>\n" {
		t.Fatalf("index.html = %q after the execution that writes it", got)
	}

	// A run that writes no page starts without the earlier one, leaves none,
	// and records the output absent.
	f.setPages(t, "")
	absent := f.executed(t, "execution that writes no page", f.run(t, SchedulerConfig{}))[0]
	assertNoFile(t, "execution that writes no page", index)
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(absent.CacheKey)
	if err != nil || entry == nil {
		t.Fatalf("no entry under %s (err=%v)", absent.CacheKey, err)
	}
	if recorded, ok := entry.Output("index"); !ok || recorded.State != store.TaskOutputEmpty {
		t.Fatalf("index recorded as %+v, want the output recorded absent", recorded)
	}

	// The hit of the present record writes the page back.
	f.setPages(t, "index")
	f.hit(t, "restore of the present record", f.run(t, SchedulerConfig{}))
	if got := readFileAt(t, index); got != "<h1>index</h1>\n" {
		t.Fatalf("index.html = %q after the restore of the present record", got)
	}

	// The hit of the absent record removes the page the restore before left,
	// as the execution that produced the record left none.
	f.setPages(t, "")
	f.hit(t, "restore of the absent record", f.run(t, SchedulerConfig{}))
	assertNoFile(t, "restore of the absent record", index)

	if got := invocationLog(t, f.logPath); len(got) != 2 {
		t.Fatalf("invocations = %v, want the two executions", got)
	}
}

// A directory at an emptied file output's path is no file the task wrote, and
// it is never removed with what it holds: the execution fails before the tool
// runs, and a restore that would remove it is not served, so the task
// executes and fails the same way.
func TestEmptiedFileOutputRefusesADirectoryAtItsPath(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-failure-to-empty-fails-the-task-before-it-runs")
	f, index := emptiedFileFixture(t)
	held := filepath.Join(index, "note.txt")
	writeFileAt(t, held, "not the task's\n")

	result := f.run(t, SchedulerConfig{})
	row := f.rows(t, "execution over a directory", result)[0]
	if row.Status == "success" || row.Error == nil || !strings.Contains(row.Error.Message, "is a directory") {
		t.Fatalf("execution over a directory = status %q error %+v, want a failure naming the directory", row.Status, row.Error)
	}
	if got := invocationLog(t, f.logPath); len(got) != 0 {
		t.Fatalf("invocations = %v, want none: the removal fails before the tool runs", got)
	}
	if got := readFileAt(t, held); got != "not the task's\n" {
		t.Fatalf("%s = %q, want it untouched", held, got)
	}

	// A run that caches the absence, then the same directory before its hit.
	if err := os.RemoveAll(index); err != nil {
		t.Fatal(err)
	}
	f.executed(t, "execution that writes no page", f.run(t, SchedulerConfig{}))
	writeFileAt(t, held, "not the task's\n")
	row = f.rows(t, "restore over a directory", f.run(t, SchedulerConfig{}))[0]
	if row.Status == "success" || row.ReuseKind().CacheHit() {
		t.Fatalf("restore over a directory = status %q hit %v, want the task executed and failed", row.Status, row.ReuseKind().CacheHit())
	}
	if got := readFileAt(t, held); got != "not the task's\n" {
		t.Fatalf("%s = %q, want it untouched", held, got)
	}
}

func assertNoFile(t *testing.T, leg, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: %s is there (err=%v), want it absent", leg, path, err)
	}
}

// An emptied file the Git index tracks is never removed, as a tracked file in
// an emptied directory is not (ADR 0068 point 6). The scheduler reads the
// index for an emptied file only while a file is at its path: an absent file
// has nothing to remove, so a run whose skip writes none starts no git (ADR
// 0107).
func TestEmptiedFileOutputKeepsATrackedFileAndReadsNoIndexWhenAbsent(t *testing.T) {
	spectest.Proves(t, emptyingFeature, "emptied-output-keeps-tracked-files", "an-emptied-output-that-holds-a-tracked-file-is-never-emptied")
	isolateTreeKeyedGit(t)
	f, index := emptiedFileFixture(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(f.ws.Root))
	writeTestFile(t, filepath.Join(f.ws.Root, ".gitignore"), ".gen/\n")
	treeKeyedGit(t, f.ws.Root, "init", "-q", "-b", "main")
	saved := trackedPaths
	reads := 0
	trackedPaths = func(dir string) ([]string, error) {
		reads++
		return saved(dir)
	}
	t.Cleanup(func() { trackedPaths = saved })

	// Absent: no read, before the execution and before its hit.
	f.executed(t, "execution with no file", f.run(t, SchedulerConfig{}))
	f.hit(t, "hit with no file", f.run(t, SchedulerConfig{}))
	if reads != 0 {
		t.Fatalf("index reads = %d, want none while the emptied file is absent", reads)
	}

	// Present and untracked: the index is read, and the hit removes the file.
	writeFileAt(t, index, "<h1>earlier</h1>\n")
	f.hit(t, "hit over an untracked file", f.run(t, SchedulerConfig{}))
	assertNoFile(t, "hit over an untracked file", index)
	if reads == 0 {
		t.Fatal("the index was not read for a file at the emptied path")
	}

	// Present and tracked: the execution is refused and the file stays.
	rel := emptyingProject + "/.gen/public/static/index.html"
	writeFileAt(t, index, "<h1>committed</h1>\n")
	treeKeyedGit(t, f.ws.Root, "add", "-f", rel)
	f.setPages(t, "other")
	row := f.rows(t, "execution over a tracked file", f.run(t, SchedulerConfig{}))[0]
	if row.Status != "failed" || row.Error == nil || !strings.Contains(row.Error.Message, "holds the Git-tracked file "+rel) {
		t.Fatalf("execution over a tracked file = status %q error %+v, want the tracked-file refusal", row.Status, row.Error)
	}
	if got := readFileAt(t, index); got != "<h1>committed</h1>\n" {
		t.Fatalf("index.html = %q, want the committed file kept", got)
	}
}

// skippingScript is a tool that writes index.html when pages.txt names it, and
// otherwise reports a deterministic SKIP that writes nothing, as build-infra
// does for a library. It logs one line per invocation.
func skippingScript(logPath string) string {
	return "#!/bin/sh\n" +
		"printf 'run\\n' >> " + shellQuote(logPath) + "\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"extension\":\"@test/generator\",\"job\":\"build\"}}'\n" +
		"if grep -qx index pages.txt; then\n" +
		"  mkdir -p .gen/public/static\n" +
		"  printf '<h1>index</h1>\\n' > .gen/public/static/index.html\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n" +
		"else\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"SKIP\"}}'\n" +
		"fi\n"
}

// A deterministic SKIP that writes no emptied file is cached as the file's
// absence (ADR 0107): the execution removes a file an earlier run left, the
// entry records it absent, and the next run is served from that entry and
// removes a file left again, without running the tool.
func TestASkipThatWritesNoEmptiedFileIsCachedAsItsAbsence(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-emptied-file-output-is-removed-on-execution-and-on-an-absent-restore")
	f, index := emptiedFileFixture(t)
	writeExecutable(t, f.planned[0].JobDef.Command, skippingScript(f.logPath))

	writeFileAt(t, index, "<h1>earlier</h1>\n")
	first := f.rows(t, "skip over an earlier file", f.run(t, SchedulerConfig{}))[0]
	if first.Status != "skipped" || first.ReuseKind().CacheHit() {
		t.Fatalf("skip = status %q hit %v, want an executed skip", first.Status, first.ReuseKind().CacheHit())
	}
	assertNoFile(t, "skip over an earlier file", index)
	entry, err := store.NewLocalStore(f.storeRoot).LookupTaskEntry(first.CacheKey)
	if err != nil || entry == nil {
		t.Fatalf("the skip stored no entry under %s (err=%v)", first.CacheKey, err)
	}
	if recorded, ok := entry.Output("index"); !ok || recorded.State != store.TaskOutputEmpty {
		t.Fatalf("index recorded as %+v, want the output recorded absent", recorded)
	}

	writeFileAt(t, index, "<h1>earlier</h1>\n")
	second := f.rows(t, "hit of the skip", f.run(t, SchedulerConfig{}))[0]
	if second.Status != "skipped" || !second.ReuseKind().CacheHit() {
		t.Fatalf("second run = status %q hit %v, want the skip served from the cache", second.Status, second.ReuseKind().CacheHit())
	}
	assertNoFile(t, "hit of the skip", index)
	if got := invocationLog(t, f.logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want the one execution", got)
	}
}

// A removal that fails leaves a file the run did not write, so the execution
// fails before the tool runs and stores nothing.
func TestAFailedRemovalOfAnEmptiedFileFailsTheTask(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-failure-to-empty-fails-the-task-before-it-runs")
	f, index := emptiedFileFixture(t)
	saved := removeOutputFile
	removeOutputFile = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { removeOutputFile = saved })

	writeFileAt(t, index, "<h1>earlier</h1>\n")
	row := f.rows(t, "execution over an unremovable file", f.run(t, SchedulerConfig{}))[0]
	if row.Status == "success" || row.Error == nil || !strings.Contains(row.Error.Message, "remove output file") {
		t.Fatalf("execution = status %q error %+v, want a failure naming the removal", row.Status, row.Error)
	}
	if got := invocationLog(t, f.logPath); len(got) != 0 {
		t.Fatalf("invocations = %v, want none", got)
	}
	if got := countTaskEntryDescriptors(t, f.storeRoot); got != 0 {
		t.Fatalf("entries stored = %d, want none", got)
	}
}
