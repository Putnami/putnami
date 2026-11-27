package clitest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/procstat"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
)

// Counts is what one run of the real CLI started, read from its
// run.orchestration report: the processes by origin and kind, and the git
// calls by origin and subcommand. The engine is the CLI process; jobs are the
// job processes built on the extension SDK, which report what they start.
type Counts struct {
	// Tasks is run.counts.total, the selected tasks.
	Tasks int
	// Engine and Jobs are the processes each origin started, by kind.
	Engine, Jobs map[string]int64
	// EngineGit and JobsGit are the git calls each origin made, by subcommand.
	EngineGit, JobsGit map[string]int64
	// EngineCPU is the CLI process's own CPU over the run; CPUMeasured is false
	// where the platform measures none.
	EngineCPU   time.Duration
	CPUMeasured bool
}

// Ledger is the orchestration counts of c on one line, in canonical order:
// every process kind the engine started, its git calls by subcommand, then the
// CLI starts and git calls of the jobs. The tools jobs run ("other" from jobs)
// are the jobs' work and are left out. A kind or subcommand that started
// nothing is left out.
func (c Counts) Ledger() string {
	var parts []string
	parts = append(parts, "engine "+kinds(c.Engine, protocolcli.OrchestrationKinds))
	parts = append(parts, "engine git "+subcommands(c.EngineGit))
	parts = append(parts, "jobs "+kinds(c.Jobs, []string{protocolcli.OrchestrationKindCLI, protocolcli.OrchestrationKindGit}))
	parts = append(parts, "jobs git "+subcommands(c.JobsGit))
	return strings.Join(parts, " | ")
}

func kinds(counts map[string]int64, order []string) string {
	var out []string
	for _, kind := range order {
		if n := counts[kind]; n > 0 {
			out = append(out, fmt.Sprintf("%s %d", kind, n))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, " · ")
}

func subcommands(calls map[string]int64) string {
	names := make([]string, 0, len(calls))
	for name, n := range calls {
		if n > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	slices.Sort(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("%s %d", name, calls[name]))
	}
	return strings.Join(out, " · ")
}

// OuterRunEnvironment is what a run that hosts the test may set: an outer
// Putnami run sets PUTNAMI_* variables in its jobs (an offline dependency
// flag, a credential descriptor, a proposal message), and a CI provider sets
// CI and its own variables. Each one can change what a run of a fixture
// starts.
var OuterRunEnvironment = map[string]string{
	"CI":                           "true",
	"GITHUB_ACTIONS":               "true",
	"PUTNAMI_OFFLINE_DEPENDENCIES": "1",
	"PUTNAMI_PROPOSAL_MESSAGE":     "test: outer run\n\nbody",
}

// UnsetHostRunEnvironment removes, for the test, every variable an outer run
// may set: each PUTNAMI_* variable but PUTNAMI_HOME, which places the machine's
// Putnami home and its provisioned tools, CI, and the GITHUB_* variables. A test
// that holds exact counts calls it first, then sets what its fixture needs, so
// it reads the same counts on a developer's machine, in a Putnami job and in
// CI.
func UnsetHostRunEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "PUTNAMI_HOME" ||
			name != "CI" && !strings.HasPrefix(name, "PUTNAMI_") && !strings.HasPrefix(name, "GITHUB_") {
			continue
		}
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// RunCounts drives one invocation of the real CLI in the workspace at wsRoot
// with --output=json, fails the test unless it exits 0, and returns the counts
// of its run.orchestration report.
//
// The report of the first run of a process covers the process since it
// started; a test that compares counts first starts one orchestration clock
// (scheduler.NewOrchestrationClock) in its TestMain, so each run reports its
// own share only. A test that holds exact counts calls ForgetTaskHistory
// first.
func RunCounts(t *testing.T, wsRoot string, args ...string) Counts {
	t.Helper()
	antedateGitState(t, wsRoot)
	code, out := RunGateArgs(t, wsRoot, append(args, "--output=json")...)
	if code != 0 {
		t.Fatalf("putnami %s exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	return ParseCounts(t, out)
}

// ExecutedTasks drives one invocation of the real CLI in the workspace at
// wsRoot with --output=jsonl, fails the test unless it exits 0, and returns, in
// the order they ended, the keys of the tasks it executed: every task record
// that reused no result, whatever its verdict. ended is the number of task
// records the run ended, executed or reused.
func ExecutedTasks(t *testing.T, wsRoot string, args ...string) (executed []string, ended int) {
	t.Helper()
	code, out := RunGateArgs(t, wsRoot, append(args, "--output=jsonl")...)
	if code != 0 {
		t.Fatalf("putnami %s exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	executed = []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record protocolcli.SessionStreamRecord
		if json.Unmarshal([]byte(line), &record) != nil || record.Record != protocolcli.RecordTaskEnd || record.Task == nil {
			continue
		}
		ended++
		if record.Task.Reuse == protocolcli.TaskReuseNone {
			executed = append(executed, record.Task.Identity.Key)
		}
	}
	return executed, ended
}

// ForgetTaskHistory removes the task history of the workspace at wsRoot.
//
// A run dispatches its ready tasks longest first by the durations the runs
// before it measured (scheduler.TaskStatsPath), and those durations follow the
// machine's load. Which task runs first decides which input digests a task's
// outputs invalidate before another task keys them, and so how many times a
// digest lists its Git candidates. Without the history, ready tasks dispatch in
// key order; on one worker they also execute in that order, whatever the load
// of the runs before it.
func ForgetTaskHistory(t *testing.T, wsRoot string) {
	t.Helper()
	if err := os.Remove(scheduler.TaskStatsPath(wsRoot)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// PlanCounts drives one invocation of the real CLI in the workspace at wsRoot
// with --dry-run, which plans the run and executes nothing, fails the test
// unless it exits 0, and returns what the process-wide ledger
// (go.putnami.dev/sdk/extension/procstat) counted while it ran. A plan reports
// no run.orchestration, so the ledger is read around it: the test binary runs
// the CLI in its own process, one invocation at a time. Tasks and the CPU are
// not measured.
func PlanCounts(t *testing.T, wsRoot string, args ...string) Counts {
	t.Helper()
	antedateGitState(t, wsRoot)
	engine, jobs := procstat.Take(), procstat.TakeReported()
	code, out := RunGateArgs(t, wsRoot, append(args, "--dry-run")...)
	if code != 0 {
		t.Fatalf("putnami %s --dry-run exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	afterEngine, afterJobs := procstat.Take(), procstat.TakeReported()
	counts := Counts{
		Engine: map[string]int64{}, Jobs: map[string]int64{},
		EngineGit: map[string]int64{}, JobsGit: map[string]int64{},
	}
	for _, kind := range procstat.Kinds {
		counts.Engine[string(kind)] = afterEngine.Of(kind).Starts - engine.Of(kind).Starts
		counts.Jobs[string(kind)] = afterJobs.Of(kind).Starts - jobs.Of(kind).Starts
	}
	for subcommand, calls := range afterEngine.Git {
		counts.EngineGit[subcommand] = calls - engine.Git[subcommand]
	}
	for subcommand, calls := range afterJobs.Git {
		counts.JobsGit[subcommand] = calls - jobs.Git[subcommand]
	}
	return counts
}

// antedateGitState dates the repository's configuration and HEAD at wsRoot an
// hour back, as a clone's are when a run starts. A process keeps a reading of
// Git's configuration only when its files changed no less than a second
// before (tooling/cli ADR 0112); a fixture built a moment before its run
// would read it again at each in-process read of the cut.
func antedateGitState(t *testing.T, wsRoot string) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	for _, file := range []string{"config", "HEAD"} {
		if err := os.Chtimes(filepath.Join(wsRoot, ".git", file), past, past); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// ParseCounts reads the counts of the --output=json document in out.
func ParseCounts(t *testing.T, out string) Counts {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("the run printed no JSON document:\n%s", out)
	}
	var document protocolcli.ResultV2
	if err := json.NewDecoder(strings.NewReader(out[start:])).Decode(&document); err != nil {
		t.Fatalf("the run printed no JSON document (%v):\n%s", err, out)
	}
	if document.Run == nil || document.Run.Orchestration == nil {
		t.Fatalf("the run reports no run.orchestration:\n%s", out)
	}
	report := document.Run.Orchestration
	counts := Counts{
		Tasks:  document.Run.Counts.Total,
		Engine: map[string]int64{}, Jobs: map[string]int64{},
		EngineGit: map[string]int64{}, JobsGit: map[string]int64{},
	}
	for _, entry := range report.Processes {
		counts.byOrigin(t, entry.Origin, counts.Engine, counts.Jobs)[entry.Kind] += entry.Count
	}
	for _, entry := range report.Git {
		counts.byOrigin(t, entry.Origin, counts.EngineGit, counts.JobsGit)[entry.Subcommand] += entry.Count
	}
	if report.CPU != nil {
		counts.CPUMeasured = true
		counts.EngineCPU = time.Duration(report.CPU.EngineMs) * time.Millisecond
	}
	return counts
}

func (Counts) byOrigin(t *testing.T, origin string, engine, jobs map[string]int64) map[string]int64 {
	t.Helper()
	switch origin {
	case protocolcli.OrchestrationOriginEngine:
		return engine
	case protocolcli.OrchestrationOriginJobs:
		return jobs
	}
	t.Fatalf("run.orchestration names an unknown origin %q", origin)
	return nil
}

// countsBatchScript answers a batch for every project it was handed, and a
// single task for its own project.
const countsBatchScript = `if [ -n "$PUTNAMI_SELECTED_PROJECTS" ]; then
  out='{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":['
  sep=''
  IFS=','
  for p in $PUTNAMI_SELECTED_PROJECT_IDS; do
    out="$out$sep{\"projectId\":\"$p\",\"status\":\"OK\"}"
    sep=','
  done
  printf '%s]}}}\n' "$out"
else
  printf '%s\n' '{"v":2,"type":"result","data":{"status":"OK"}}'
fi
`

// CountsFixture writes a workspace of projects projects, each running three
// trivial cacheable shell tasks, one per lint, test and build, and commits it
// on main. Nothing in it starts a process but the engine and /bin/sh, so its
// counts are the engine's alone.
//
// With batchable, the tasks declare a batch policy, as Go tests and lint do:
// the scheduler groups ready tasks into one process, so the number of job
// processes depends on the host's workers and no count holds it.
func CountsFixture(t *testing.T, projects int, batchable bool) string {
	t.Helper()
	return countsFixture(t, projects, batchable, false, false)
}

// WritingCountsFixture is CountsFixture whose tasks write: each keys on the
// Git candidates of its project (`git:**`) and writes one declared output
// under its command output directory, below the workspace's .putnami, which
// the workspace's .gitignore excludes. A write there changes no candidate, so
// it moves no digest of the candidates a later task keys on, and the git calls
// of a run are those of a run whose tasks write nothing.
func WritingCountsFixture(t *testing.T, projects int) string {
	t.Helper()
	return countsFixture(t, projects, false, true, false)
}

// SourceBoundCountsFixture is CountsFixture whose tasks declare
// cache.sourceBound: each entry is stored and looked up at its cache key
// bound to its project's source binding. The workspace's .gitignore excludes
// the build stamp the binding is written to.
func SourceBoundCountsFixture(t *testing.T, projects int) string {
	t.Helper()
	return countsFixture(t, projects, false, false, true)
}

func countsFixture(t *testing.T, projects int, batchable, writes, sourceBound bool) string {
	t.Helper()
	root := t.TempDir()
	includes := []string{"extension"}
	for i := range projects {
		project := fmt.Sprintf("p%03d", i)
		includes = append(includes, "apps/"+project)
		WriteFile(t, filepath.Join(root, "apps", project, "putnami.json"), `{"name":"`+project+`","extensions":["@fixture/counts"]}`)
		WriteFile(t, filepath.Join(root, "apps", project, "src.txt"), project+"\n")
	}
	workspace, err := json.Marshal(struct {
		Name     string   `json:"name"`
		Includes []string `json:"includes"`
	}{"counts-fixture", includes})
	if err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(root, "putnami.workspace.json"), string(workspace))
	WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/counts"}`)
	enabled := true
	commands := map[string]extensionproto.CommandDefinition{}
	tasks := map[string]extensionproto.TaskDefinition{}
	for _, command := range []string{"lint", "test", "build"} {
		commands[command] = extensionproto.CommandDefinition{ActivationFiles: []string{"src.txt"}, Run: []extensionproto.PipelineStep{{ID: command, Task: command}}}
		tasks[command] = extensionproto.TaskDefinition{
			Kind: "command", Command: "/bin/sh", Args: []string{"-c", "exit 0"}, TimeoutMs: -1,
			Reads:    []extensionproto.ResourceRef{{ID: extensionproto.ResourceIDSources}},
			Cache:    &extensionproto.TaskCachePolicy{Enabled: &enabled, Deterministic: true, NoOutput: true},
			Declares: &extensionproto.TaskDeclaration{},
		}
		if writes {
			task := tasks[command]
			task.Args = []string{"-c", `printf '%s\n' "$0" > "$1"`, command, "{outputRoot}/report.txt"}
			task.Inputs = map[string]extensionproto.TaskInputPort{"candidates": {From: "project", Files: []string{"git:**"}}}
			task.Cache = &extensionproto.TaskCachePolicy{Enabled: &enabled, Deterministic: true}
			task.Declares = &extensionproto.TaskDeclaration{Outputs: map[string]extensionproto.DeclaredOutput{
				"report": {Kind: "file", Root: "command-output", Path: "report.txt"},
			}}
			tasks[command] = task
		}
		if sourceBound {
			task := tasks[command]
			task.Cache.SourceBound = true
			tasks[command] = task
		}
		if batchable {
			task := tasks[command]
			task.Args = []string{"{extensionRoot}/batch.sh"}
			task.Batchable = &extensionproto.TaskBatchPolicy{Tool: "sh"}
			tasks[command] = task
		}
	}
	manifest := extensionproto.Manifest{
		Name: "@fixture/counts", Version: "1.0.0", CLIContract: protocolcli.CommandKindContract,
		Commands: commands, Tasks: tasks,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	WriteFile(t, filepath.Join(root, "extension", "batch.sh"), countsBatchScript)
	switch {
	case writes:
		WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n")
	case sourceBound:
		WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	}
	InitGitRepo(t, root)
	return root
}
