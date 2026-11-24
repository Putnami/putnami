package machine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

func TestSummary_CarriesTheOrchestrationReport(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-engine-and-sdk-jobs-count-their-processes-by-kind-and-git-by-subcommand")
	session := &jobs.SessionResult{
		Status: jobs.TaskCounts{Succeeded: 4},
		Orchestration: &jobs.OrchestrationSnapshot{
			// Recorded out of canonical order: the projection orders them.
			Phases: []jobs.OrchestrationPhaseWall{
				{Phase: protocolcli.OrchestrationPhasePlan, Wall: 30 * time.Millisecond},
				{Phase: protocolcli.OrchestrationPhaseWorkspaceLoad, Wall: 40 * time.Millisecond},
			},
			Engine: jobs.ProcessLedger{
				Kinds: []jobs.ProcessCount{{Kind: "job", Starts: 3}, {Kind: "control", Starts: 2, CPU: 90 * time.Millisecond, Measured: 2}, {Kind: "git", Starts: 5, CPU: 0, Measured: 5}, {Kind: "other"}},
				Git:   map[string]int64{"rev-parse": 4, "ls-files": 1},
			},
			Jobs: jobs.ProcessLedger{
				Kinds: []jobs.ProcessCount{{Kind: "cli", Starts: 1, CPU: 300 * time.Millisecond, Measured: 1}, {Kind: "git", Starts: 2, CPU: 30 * time.Millisecond, Measured: 2}, {Kind: "other", Starts: 3, CPU: 9 * time.Second, Measured: 3}},
				Git:   map[string]int64{"rev-parse": 2},
			},
			CPUMeasured:         true,
			EngineCPU:           1200 * time.Millisecond,
			ExtensionCPU:        180 * time.Millisecond,
			RunCPU:              11 * time.Second,
			UnreportedProcesses: 1,
		},
	}
	summary := Run{Session: session}.Summary(5000)
	got := summary.Orchestration
	if got == nil {
		t.Fatal("no orchestration report")
	}
	if len(got.Phases) != 2 || got.Phases[0].Phase != protocolcli.OrchestrationPhaseWorkspaceLoad || got.Phases[1].WallMs != 30 {
		t.Errorf("phases = %+v", got.Phases)
	}
	ms := func(v int64) *int64 { return &v }
	// A kind with no measured process has no cpuMs; one measured at under a
	// millisecond has 0.
	wantProcesses := []protocolcli.OrchestrationProcesses{
		{Origin: "engine", Kind: "job", Count: 3},
		{Origin: "engine", Kind: "control", Count: 2, CPUMs: ms(90)},
		{Origin: "engine", Kind: "git", Count: 5, CPUMs: ms(0)},
		{Origin: "jobs", Kind: "cli", Count: 1, CPUMs: ms(300)},
		{Origin: "jobs", Kind: "git", Count: 2, CPUMs: ms(30)},
		{Origin: "jobs", Kind: "other", Count: 3, CPUMs: ms(9000)},
	}
	if !reflect.DeepEqual(got.Processes, wantProcesses) {
		t.Errorf("processes = %+v, want %+v", got.Processes, wantProcesses)
	}
	wantGit := []protocolcli.OrchestrationGitCalls{
		{Origin: "engine", Subcommand: "ls-files", Count: 1},
		{Origin: "engine", Subcommand: "rev-parse", Count: 4},
		{Origin: "jobs", Subcommand: "rev-parse", Count: 2},
	}
	for i := range wantGit {
		if i >= len(got.Git) || got.Git[i] != wantGit[i] {
			t.Errorf("git = %+v, want %+v", got.Git, wantGit)
			break
		}
	}
	// Orchestration: the engine, the extensions' own CPU, control, the CLI
	// starts and git of both origins; never the tools jobs run.
	want := protocolcli.OrchestrationCPU{
		EngineMs: 1200, ExtensionMs: 180, ControlMs: 90, CLIMs: 300, GitMs: 30,
		TotalMs: 1800, RunMs: 11000, SharePermille: ms(163), PerTaskMs: ms(450), UnreportedProcesses: 1,
	}
	if got.CPU == nil || !reflect.DeepEqual(*got.CPU, want) {
		t.Errorf("cpu = %+v, want %+v", got.CPU, want)
	}

	// The document the session and the result carry validates against the
	// contract.
	envelope := struct {
		ProtocolVersion int                    `json:"protocolVersion"`
		Command         string                 `json:"command"`
		Status          string                 `json:"status"`
		ExitCode        int                    `json:"exitCode"`
		Run             protocolcli.RunSummary `json:"run"`
	}{2, "build", "success", 0, summary}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentResultEnvelope, data); len(violations) != 0 {
		t.Fatalf("violations = %v", violations)
	}
}

func TestSummary_OmitsAnUnmeasuredOrchestrationReport(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	if got := (Run{Session: &jobs.SessionResult{}}).Summary(0).Orchestration; got != nil {
		t.Errorf("a run without a report = %+v, want none", got)
	}
	unmeasured := &jobs.SessionResult{Orchestration: &jobs.OrchestrationSnapshot{
		Phases: []jobs.OrchestrationPhaseWall{{Phase: protocolcli.OrchestrationPhaseProbe, Wall: time.Millisecond}},
	}}
	got := Run{Session: unmeasured}.Summary(0).Orchestration
	if got == nil || got.CPU != nil || len(got.Processes) != 0 {
		t.Errorf("a platform without process CPU = %+v, want phases only", got)
	}
	idle := &jobs.SessionResult{Status: jobs.TaskCounts{Succeeded: 40}, Orchestration: &jobs.OrchestrationSnapshot{
		CPUMeasured: true, EngineCPU: 10 * time.Millisecond, RunCPU: 50 * time.Second,
	}}
	cpu := Run{Session: idle}.Summary(0).Orchestration.CPU
	if cpu == nil || cpu.SharePermille == nil || *cpu.SharePermille != 0 || cpu.PerTaskMs == nil || *cpu.PerTaskMs != 0 {
		t.Errorf("a share and a per-task cost that truncate to 0 = %+v, want both present at 0", cpu)
	}
}

func TestTaskRecord_CarriesTheStepsATaskEntered(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	var waits jobs.TaskWaits
	waits.AddKey(3 * time.Millisecond)
	waits.AddLockWait(200 * time.Microsecond)
	record := TaskRecord(Task{Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Timing: jobs.TaskTiming{Waits: waits}}})
	o := record.Orchestration
	if o == nil || o.KeyMs == nil || *o.KeyMs != 3 || o.LockWaitMs == nil || *o.LockWaitMs != 0 || o.RestoreMs != nil {
		t.Fatalf("orchestration = %+v: a step under a millisecond is present at 0, a step never entered is absent", o)
	}
	if none := TaskRecord(Task{Result: jobs.TaskResult{Status: jobs.TaskStatusSkipped}}); none.Orchestration != nil {
		t.Errorf("a task that entered no step = %+v, want none", none.Orchestration)
	}
}
