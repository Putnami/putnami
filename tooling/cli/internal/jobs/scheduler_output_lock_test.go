//go:build unix

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// outputRaceFixture is one workspace two sessions share. The FIRST execution of
// its job writes the start of report.txt, announces itself on a FIFO, waits for
// the test's release, and writes the end. Any later execution records the
// report it found before writing its own. The preBuild-hook cases do the same
// with proj/.gen/hook.txt.
type outputRaceFixture struct {
	root     string
	extDir   string
	outDir   string
	hookFile string
	started  string
	release  string
	seen     string
	endState string
	project  *workspace.Project
	ws       *workspace.Workspace
}

const outputRaceSessionA = "20260925-192012-876307"

func newOutputRaceFixture(t *testing.T) *outputRaceFixture {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	root := t.TempDir()
	// The extension is installed in a directory of its own: its installed tree
	// names it in every key, so it must not hold the workspace, whose files the
	// sessions write while they run.
	f := &outputRaceFixture{
		root:     root,
		extDir:   t.TempDir(),
		outDir:   filepath.Join(root, ".putnami", "out", "proj", "test"),
		hookFile: filepath.Join(root, "proj", ".gen", "hook.txt"),
		started:  filepath.Join(root, "started.fifo"),
		release:  filepath.Join(root, "release"),
		seen:     filepath.Join(root, "seen-by-second"),
		endState: filepath.Join(root, "first-end-state"),
		project:  &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
	}
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(root, "proj", "input.txt"), "input\n")
	if err := syscall.Mkfifo(f.started, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	f.ws = workspace.NewWorkspace(root, nil, []*workspace.Project{f.project})
	f.ws.Name = "output-lock"
	t.Cleanup(func() { _ = os.WriteFile(f.release, []byte("release"), 0o644) })
	return f
}

// releaseWaitScript waits until the test writes releasePath, and ends early
// only when the test process is gone. No poll count or clock bounds it: the
// test's own guards decide what a hang is.
func releaseWaitScript(releasePath string, testPID int) string {
	return "while [ ! -f " + shellQuote(releasePath) + " ]; do\n" +
		fmt.Sprintf("  if ! kill -0 %d 2>/dev/null; then exit 125; fi\n", testPID) +
		"  sleep 0.01\n" +
		"done\n"
}

// blockingWriter is a script that begins file, announces itself, waits for the
// release, ends file, then records what file holds at its own end.
func (f *outputRaceFixture) blockingWriter(t *testing.T, name, file string) string {
	t.Helper()
	quoted := shellQuote(file)
	path := filepath.Join(f.root, name)
	writeExecutable(t, path, "#!/bin/sh\n"+
		"mkdir -p "+shellQuote(filepath.Dir(file))+"\n"+
		"printf 'a-begin\\n' > "+quoted+"\n"+
		"printf 'started\\n' > "+shellQuote(f.started)+"\n"+
		releaseWaitScript(f.release, os.Getpid())+
		"printf 'a-end\\n' >> "+quoted+"\n"+
		"cp "+quoted+" "+shellQuote(f.endState)+"\n")
	return path
}

// blockingScript is the first execution, writing report.txt.
func (f *outputRaceFixture) blockingScript(t *testing.T) string {
	t.Helper()
	return f.blockingWriter(t, "blocking.sh", filepath.Join(f.outDir, "report.txt"))
}

// blockingHook is a preBuild hook that holds its preparation, writing
// proj/.gen/hook.txt.
func (f *outputRaceFixture) blockingHook(t *testing.T) string {
	t.Helper()
	return f.blockingWriter(t, "blocking-hook.sh", f.hookFile)
}

// writingHook is a preBuild hook that overwrites proj/.gen/hook.txt at once.
func (f *outputRaceFixture) writingHook(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.root, "writing-hook.sh")
	writeExecutable(t, path, "#!/bin/sh\n"+
		"mkdir -p "+shellQuote(filepath.Dir(f.hookFile))+"\n"+
		"printf 'b\\n' > "+shellQuote(f.hookFile)+"\n")
	return path
}

// noopScript is a job command that writes nothing.
func (f *outputRaceFixture) noopScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.root, "noop.sh")
	writeExecutable(t, path, "#!/bin/sh\n")
	return path
}

// withPreBuildHook gives job's extension a preBuild hook running script. Its
// timeout is far beyond every guard of the test, so it never decides a case.
func withPreBuildHook(job *ScheduledJob, script string) *ScheduledJob {
	job.Extension.Hooks = &extension.ManifestHooks{PreBuild: &extension.HookDefinition{
		Kind: "command", Command: script, TimeoutMs: int(time.Hour / time.Millisecond),
	}}
	return job
}

// secondScript records the report it finds, then writes its own.
func (f *outputRaceFixture) secondScript(t *testing.T, content string) string {
	t.Helper()
	report := shellQuote(filepath.Join(f.outDir, "report.txt"))
	path := filepath.Join(f.root, "second-"+content+".sh")
	writeExecutable(t, path, "#!/bin/sh\n"+
		"if [ -f "+report+" ]; then cp "+report+" "+shellQuote(f.seen)+"; fi\n"+
		"printf '"+content+"\\n' > "+report+"\n")
	return path
}

// job is proj's test~report task. A cacheable job declares report.txt as its
// command-output file; an uncached one runs the same way without an entry.
func (f *outputRaceFixture) job(command string, cacheable bool) *ScheduledJob {
	return f.jobNamed("test~report", command, cacheable)
}

// jobNamed is job under another command~step name.
func (f *outputRaceFixture) jobNamed(name, command string, cacheable bool) *ScheduledJob {
	const task = "test-report"
	return &ScheduledJob{
		Project: f.project,
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/test", Path: f.extDir,
			Tasks: map[string]extension.TaskDefinition{
				task: {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"report": fileDeclaration(extension.OutputRootCommandOutput, "report.txt", false),
				}}},
			},
		},
		Step: &extension.PipelineStep{Task: task},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@putnami/test",
			Command:       command,
			TimeoutMs:     unboundedJobTimeoutMs,
			Cache:         cacheable,
			FilePatterns:  []string{"input.txt"},
		},
	}
}

func (f *outputRaceFixture) scheduler(job *ScheduledJob, session string, cache *store.CacheManager) *Scheduler {
	return f.plan(session, cache, job)
}

// plan is a session running jobs one at a time.
func (f *outputRaceFixture) plan(session string, cache *store.CacheManager, jobs ...*ScheduledJob) *Scheduler {
	return newScheduler(f.ws, jobs, nil, SchedulerConfig{MaxParallel: 1, SessionID: session}, &mockRenderer{}, cache)
}

// runAsync runs a scheduler on its own goroutine.
func runAsync(ctx context.Context, s *Scheduler) <-chan *SchedulerResult {
	done := make(chan *SchedulerResult, 1)
	go func() { done <- s.Run(ctx) }()
	return done
}

// awaitFirstStarted blocks until the first execution announced itself on the
// FIFO, or fails when that session ended without doing so.
func (f *outputRaceFixture) awaitFirstStarted(t *testing.T, first <-chan *SchedulerResult) {
	t.Helper()
	started := make(chan error, 1)
	go func() {
		_, err := os.ReadFile(f.started)
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("read start FIFO: %v", err)
		}
	case result := <-first:
		t.Fatalf("the first session ended before its job started: %+v", result.Results)
	case <-time.After(answerWaitBudget):
		t.Fatal("the first session's job never started")
	}
}

// awaitSecondWaits blocks until the second session reports that it waits for
// the first session's output lock, and fails when it finished without waiting —
// which is exactly the race: it wrote while the first execution was writing.
func awaitSecondWaits(t *testing.T, waits <-chan string, second <-chan *SchedulerResult) string {
	t.Helper()
	select {
	case message := <-waits:
		return message
	case result := <-second:
		t.Fatalf("the second session finished while the first held the task output: %+v", result.Results)
	case <-time.After(answerWaitBudget):
		t.Fatal("the second session neither waited nor finished")
	}
	return ""
}

func requireSuccess(t *testing.T, name string, result *SchedulerResult, key string) *JobResult {
	t.Helper()
	job := result.Results[key]
	if job == nil || job.Status != "success" {
		t.Fatalf("%s session result = %+v, want success", name, job)
	}
	return job
}

// TestScheduler_SecondSessionWaitsForTheTaskOutputHolder reproduces the
// concurrent-session race at the executor: two sessions of one workspace run
// the same project task, whose cache keys differ, so nothing coalesces them.
// The second execution must not start while the first writes the shared
// output directory, must name the first session, and must observe the first
// execution's complete output, never a partial one.
func TestScheduler_SecondSessionWaitsForTheTaskOutputHolder(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"a-second-session-waits-for-the-task-output-holder")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	firstJob := f.job(f.blockingScript(t), false)
	first := runAsync(ctx, f.scheduler(firstJob, outputRaceSessionA, nil))
	f.awaitFirstStarted(t, first)

	waits := make(chan string, 1)
	secondScheduler := f.scheduler(f.job(f.secondScript(t, "b"), false), "20260925-192013-000001", nil)
	secondScheduler.onOutputLockWait = func(message string) { waits <- message }
	second := runAsync(ctx, secondScheduler)

	message := awaitSecondWaits(t, waits, second)
	for _, want := range []string{"session " + outputRaceSessionA, "task /proj:test~report", ".putnami/out/proj/test"} {
		if !strings.Contains(message, want) {
			t.Errorf("wait line %q does not name %q", message, want)
		}
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, "first", <-first, firstJob.Key())
	requireSuccess(t, "second", <-second, firstJob.Key())
	if got := readFileAt(t, f.seen); got != "a-begin\na-end\n" {
		t.Fatalf("the second execution observed %q, want the first execution's complete output", got)
	}
	if got := readFileAt(t, f.endState); got != "a-begin\na-end\n" {
		t.Fatalf("the first execution ended holding %q, want only its own writes", got)
	}
}

// TestScheduler_RestoreWaitsForTheTaskOutputHolder is the cache-hit half of the
// race: a session served from cache restores into the output directory another
// session is executing into. The restore must wait for the execution, so the
// executing task never has its output replaced mid-run.
func TestScheduler_RestoreWaitsForTheTaskOutputHolder(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"a-restore-waits-for-the-task-output-holder")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(f.root, "store")))

	// Warm the entry the restoring session will hit.
	cachedJob := f.job(f.secondScript(t, "cached"), true)
	requireSuccess(t, "warming", f.scheduler(cachedJob, "20260925-192011-000000", cache).Run(ctx), cachedJob.Key())
	_ = os.Remove(f.seen)

	firstJob := f.job(f.blockingScript(t), false)
	first := runAsync(ctx, f.scheduler(firstJob, outputRaceSessionA, cache))
	f.awaitFirstStarted(t, first)

	waits := make(chan string, 1)
	restoring := f.scheduler(f.job(f.secondScript(t, "cached"), true), "20260925-192013-000001", cache)
	restoring.onOutputLockWait = func(message string) { waits <- message }
	second := runAsync(ctx, restoring)

	message := awaitSecondWaits(t, waits, second)
	if !strings.Contains(message, "session "+outputRaceSessionA) {
		t.Errorf("wait line %q does not name the executing session", message)
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, "executing", <-first, firstJob.Key())
	restored := requireSuccess(t, "restoring", <-second, firstJob.Key())
	if !restored.CacheHit {
		t.Fatalf("the restoring session executed instead of restoring: %+v", restored)
	}
	if got := readFileAt(t, f.endState); got != "a-begin\na-end\n" {
		t.Fatalf("the executing task ended holding %q: a restore replaced its output mid-run", got)
	}
	if got := readFileAt(t, filepath.Join(f.outDir, "report.txt")); got != "cached\n" {
		t.Fatalf("report.txt = %q after both sessions, want the restore that ran last", got)
	}
	if _, err := os.Stat(f.seen); !os.IsNotExist(err) {
		t.Fatalf("the restoring session executed its job (stat seen: %v)", err)
	}
}

// TestScheduler_PreBuildHookWaitsForTheProjectHolder is the preparation half
// of the race: two sessions run different commands of one project, and each
// command's extension declares a preBuild hook that writes the project's .gen.
// Their command-output keys differ, so only the project key the preparation
// takes keeps the second hook from rewriting .gen while the first one writes
// it.
func TestScheduler_PreBuildHookWaitsForTheProjectHolder(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"a-second-session-waits-for-the-task-output-holder")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	noop := f.noopScript(t)
	firstJob := withPreBuildHook(f.job(noop, false), f.blockingHook(t))
	first := runAsync(ctx, f.scheduler(firstJob, outputRaceSessionA, nil))
	f.awaitFirstStarted(t, first)

	waits := make(chan string, 1)
	secondJob := withPreBuildHook(f.jobNamed("build~report", noop, false), f.writingHook(t))
	secondScheduler := f.scheduler(secondJob, "20260925-192013-000001", nil)
	secondScheduler.onOutputLockWait = func(message string) { waits <- message }
	second := runAsync(ctx, secondScheduler)

	message := awaitSecondWaits(t, waits, second)
	for _, want := range []string{"session " + outputRaceSessionA, "task /proj:test~report", "the project-rooted outputs of proj"} {
		if !strings.Contains(message, want) {
			t.Errorf("wait line %q does not name %q", message, want)
		}
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, "first", <-first, firstJob.Key())
	requireSuccess(t, "second", <-second, secondJob.Key())
	if got := readFileAt(t, f.endState); got != "a-begin\na-end\n" {
		t.Fatalf("the first hook ended holding %q: another session's hook wrote .gen while it ran", got)
	}
	if got := readFileAt(t, f.hookFile); got != "b\n" {
		t.Fatalf("hook.txt = %q after both sessions, want the hook that ran last", got)
	}
}

// TestScheduler_AnExecutionReleasesItsPreparationKey pins where the project
// key a preparation takes ends: before the subprocess starts. A long-running
// execution whose declaration writes no project-rooted output, such as a
// serve, never keeps another session's preparation out of its project.
func TestScheduler_AnExecutionReleasesItsPreparationKey(t *testing.T) {
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The first execution of proj in its session takes the project key to
	// stamp it, then blocks in its subprocess.
	firstJob := f.job(f.blockingScript(t), false)
	first := runAsync(ctx, f.scheduler(firstJob, outputRaceSessionA, nil))
	f.awaitFirstStarted(t, first)

	waits := make(chan string, 1)
	secondJob := withPreBuildHook(f.jobNamed("build~report", f.noopScript(t), false), f.writingHook(t))
	secondScheduler := f.scheduler(secondJob, "20260925-192013-000001", nil)
	secondScheduler.onOutputLockWait = func(message string) { waits <- message }
	second := runAsync(ctx, secondScheduler)

	select {
	case message := <-waits:
		t.Fatalf("a preparation waited for another session's running subprocess: %s", message)
	case result := <-second:
		requireSuccess(t, "second", result, secondJob.Key())
	case <-time.After(answerWaitBudget):
		t.Fatal("the second session neither finished nor waited")
	}
	if got := readFileAt(t, f.hookFile); got != "b\n" {
		t.Fatalf("hook.txt = %q, want the second session's hook to have run", got)
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireSuccess(t, "first", <-first, firstJob.Key())
}

// genData is proj's generated file the gen writers of these cases own.
func (f *outputRaceFixture) genData() string {
	return filepath.Join(f.root, "proj", ".gen", "data.txt")
}

// genDataOutputs declares proj's .gen/data.txt as a project-rooted output, the
// way a generate task declares .gen.
func genDataOutputs() map[string]extension.DeclaredOutput {
	return map[string]extension.DeclaredOutput{
		"gen": fileDeclaration(extension.OutputRootProject, ".gen/data.txt", true),
	}
}

// genReads are the resources a build step reads: proj's gen and its sources.
func genReads() []extension.ResourceRef {
	return []extension.ResourceRef{{ID: "gen"}, {ID: extension.ResourceIDSources}}
}

// resourceJob is proj's name task running command: its task declares
// outputs, and its definition reads reads.
func (f *outputRaceFixture) resourceJob(name, command string, outputs map[string]extension.DeclaredOutput, reads []extension.ResourceRef) *ScheduledJob {
	task := strings.ReplaceAll(name, "~", "-")
	return &ScheduledJob{
		Project: f.project,
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/test", Path: f.extDir,
			Tasks: map[string]extension.TaskDefinition{
				task: {Declares: &extension.TaskDeclaration{Outputs: outputs}},
			},
		},
		Step: &extension.PipelineStep{Task: task},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@putnami/test",
			Command:       command,
			TimeoutMs:     unboundedJobTimeoutMs,
			FilePatterns:  []string{"input.txt"},
			Reads:         reads,
		},
	}
}

// awaitStarted blocks until a job announced itself on fifo, or fails when its
// session ended without doing so.
func awaitStarted(t *testing.T, fifo string, session <-chan *SchedulerResult, what string) {
	t.Helper()
	started := make(chan error, 1)
	go func() {
		_, err := os.ReadFile(fifo)
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("read start FIFO: %v", err)
		}
	case result := <-session:
		t.Fatalf("%s: the session ended before the job started: %+v", what, result.Results)
	case <-time.After(answerWaitBudget):
		t.Fatalf("%s never started", what)
	}
}

// TestScheduler_AReaderWaitsForAnotherSessionsWriter reproduces the read race
// at the executor. The second session generated proj's .gen and released it;
// before its build step reads .gen, the first session's generate, under
// another cache key, takes the project and starts writing .gen. The build step
// must wait for that writer, name it, and read its complete output.
func TestScheduler_AReaderWaitsForAnotherSessionsWriter(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-reader-and-another-sessions-writer-exclude-each-other")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	heldStarted := filepath.Join(f.root, "held.fifo")
	if err := syscall.Mkfifo(heldStarted, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	heldRelease := filepath.Join(f.root, "held-release")
	t.Cleanup(func() { _ = os.WriteFile(heldRelease, []byte("release"), 0o644) })
	holdScript := filepath.Join(f.root, "hold.sh")
	writeExecutable(t, holdScript, "#!/bin/sh\n"+
		"printf 'started\\n' > "+shellQuote(heldStarted)+"\n"+
		releaseWaitScript(heldRelease, os.Getpid()))
	readScript := filepath.Join(f.root, "read.sh")
	writeExecutable(t, readScript, "#!/bin/sh\ncp "+shellQuote(f.genData())+" "+shellQuote(f.seen)+"\n")

	// The second session: its generate runs and releases the project, a step
	// with no project key keeps its build step back, and the build step reads
	// .gen.
	generate := f.resourceJob("build~generate", f.noopScript(t), genDataOutputs(), nil)
	held := f.resourceJob("lint~hold", holdScript, nil, nil)
	held.DependsOn = []string{generate.Key()}
	transpile := f.resourceJob("build~transpile", readScript, nil, genReads())
	transpile.DependsOn = []string{generate.Key(), held.Key()}
	waits := make(chan string, 1)
	reading := f.plan("20260925-192013-000001", nil, generate, held, transpile)
	reading.onOutputLockWait = func(message string) { waits <- message }
	second := runAsync(ctx, reading)
	awaitStarted(t, heldStarted, second, "the second session's holding step")

	writer := f.resourceJob("test~generate", f.blockingWriter(t, "generate.sh", f.genData()), genDataOutputs(), nil)
	first := runAsync(ctx, f.plan(outputRaceSessionA, nil, writer))
	f.awaitFirstStarted(t, first)
	if err := os.WriteFile(heldRelease, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	message := awaitSecondWaits(t, waits, second)
	want := fmt.Sprintf("%s waits for session %s (pid %d, task %s), which is writing the project-rooted outputs of proj",
		transpile.Key(), outputRaceSessionA, os.Getpid(), writer.Key())
	if message != want {
		t.Errorf("wait line = %q, want %q", message, want)
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, "writing", <-first, writer.Key())
	requireSuccess(t, "reading", <-second, transpile.Key())
	if got := readFileAt(t, f.seen); got != "a-begin\na-end\n" {
		t.Fatalf("the build step read %q, want the other session's complete .gen", got)
	}
}

// TestScheduler_AWriterWaitsForAnotherSessionsReader is the other direction: a
// build step of one session reads .gen while another session's generate wants
// to write it. The generate must wait, with a line that says the project is
// being read, and the build step must read the same .gen from start to end.
func TestScheduler_AWriterWaitsForAnotherSessionsReader(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-reader-and-another-sessions-writer-exclude-each-other")
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-wait-behind-readers-names-no-holder")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writeFileAt(t, f.genData(), "initial\n")

	data := shellQuote(f.genData())
	readScript := filepath.Join(f.root, "read.sh")
	writeExecutable(t, readScript, "#!/bin/sh\n"+
		"cp "+data+" "+shellQuote(f.seen)+"\n"+
		"printf 'started\\n' > "+shellQuote(f.started)+"\n"+
		releaseWaitScript(f.release, os.Getpid())+
		"cp "+data+" "+shellQuote(f.endState)+"\n")
	writeScript := filepath.Join(f.root, "write.sh")
	writeExecutable(t, writeScript, "#!/bin/sh\nprintf 'a\\n' > "+data+"\n")

	// The reading session plans no writer of the project, and its first step
	// stamps the project, so its build step holds the project key shared.
	stamp := f.resourceJob("lint~stamp", f.noopScript(t), nil, nil)
	transpile := f.resourceJob("build~transpile", readScript, nil, genReads())
	transpile.DependsOn = []string{stamp.Key()}
	reading := runAsync(ctx, f.plan(outputRaceSessionA, nil, stamp, transpile))
	f.awaitFirstStarted(t, reading)

	waits := make(chan string, 1)
	writer := f.resourceJob("test~generate", writeScript, genDataOutputs(), nil)
	writingScheduler := f.plan("20260925-192013-000001", nil, writer)
	writingScheduler.onOutputLockWait = func(message string) { waits <- message }
	writing := runAsync(ctx, writingScheduler)

	message := awaitSecondWaits(t, waits, writing)
	if want := writer.Key() + " waits for another putnami process, which is reading the project-rooted outputs of proj"; message != want {
		t.Errorf("wait line = %q, want %q", message, want)
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, "reading", <-reading, transpile.Key())
	requireSuccess(t, "writing", <-writing, writer.Key())
	if got := readFileAt(t, f.endState); got != "initial\n" {
		t.Fatalf("the build step ended reading %q: another session wrote .gen while it read", got)
	}
	if got := readFileAt(t, f.seen); got != "initial\n" {
		t.Fatalf("the build step started reading %q, want the initial .gen", got)
	}
	if got := readFileAt(t, f.genData()); got != "a\n" {
		t.Fatalf(".gen/data.txt = %q after both sessions, want the writer that ran last", got)
	}
}
