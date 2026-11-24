package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/procstat"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func reportingJob(t *testing.T, script fixtureScript) (*workspace.Workspace, *ScheduledJob) {
	t.Helper()
	wsRoot := t.TempDir()
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "build~compile",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, script)
	return &workspace.Workspace{Root: wsRoot, Name: "test-ws"}, job
}

func TestRunJob_CountsTheJobAndAbsorbsItsProcessReport(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-engine-and-sdk-jobs-count-their-processes-by-kind-and-git-by-subcommand")
	ws, job := reportingJob(t, fixtureScript{{"print", `{"v":2,"type":"result","data":{"status":"success","data":{"binaryPath":"dist/app",` +
		`"processReport":{"selfCpuMs":7,"starts":[{"kind":"git","count":2,"cpuMs":3}],"git":[{"subcommand":"orchestration-fixture","count":2}]}}}}`}})

	engineBefore, jobsBefore := procstat.Take(), procstat.TakeReported()
	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engineAfter, jobsAfter := procstat.Take(), procstat.TakeReported()

	if got := engineAfter.Of(procstat.Job).Starts - engineBefore.Of(procstat.Job).Starts; got < 1 {
		t.Errorf("job starts = %d, want the job process counted", got)
	}
	if got := jobsAfter.Git["orchestration-fixture"] - jobsBefore.Git["orchestration-fixture"]; got != 2 {
		t.Errorf("reported git calls = %d, want 2", got)
	}
	if got := jobsAfter.Of(procstat.Git).Measured - jobsBefore.Of(procstat.Git).Measured; got != 1 {
		t.Errorf("reported git measured = %d, want 1: the report's cpuMs counts", got)
	}
	if got := jobsAfter.SelfCPU - jobsBefore.SelfCPU; got < 7*time.Millisecond {
		t.Errorf("reported self cpu grew by %s, want at least 7ms", got)
	}
	if _, kept := result.Data[runtimeproto.ProcessReportResultDataKey]; kept {
		t.Error("the process report stayed in the result data, where a cache entry would replay it")
	}
	if result.Data["binaryPath"] != "dist/app" {
		t.Errorf("result data = %v, want the job's own data kept", result.Data)
	}
	// The retained result event is what a cache entry stores and a hit replays:
	// it carries no report either.
	retained, err := json.Marshal(result.Events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(retained), runtimeproto.ProcessReportResultDataKey) {
		t.Errorf("a retained event still carries the process report: %s", retained)
	}
}

func TestAdoptSharedResult_DropsTheLeadersWaits(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	leader := &JobResult{Status: "success"}
	leader.Waits.AddKey(time.Millisecond)
	if adopted := adoptSharedResult(leader); adopted.Waits.Any() {
		t.Errorf("the follower adopted the leader's waits %+v", adopted.Waits)
	}
}

func TestRunJob_AJobWithoutAReportIsUnreported(t *testing.T) {
	ws, job := reportingJob(t, fixtureScript{{"print", `{"v":2,"type":"result","data":{"status":"success"}}`}})
	before := procstat.TakeReported()
	if _, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := procstat.TakeReported().Unreported - before.Unreported; got < 1 {
		t.Errorf("unreported = %d, want the job process counted as unreported", got)
	}
}

func TestOrchestrationClock_SumsAPhaseAndSettlesTheLedgers(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	var none *OrchestrationClock
	none.Phase(protocolcli.OrchestrationPhasePlan, time.Now())
	if none.Snapshot() != nil {
		t.Fatal("a nil clock settled a report")
	}
	clock := NewOrchestrationClock()
	started := time.Now().Add(-5 * time.Millisecond)
	clock.Phase(protocolcli.OrchestrationPhaseWorkspaceLoad, started)
	clock.Phase(protocolcli.OrchestrationPhasePlan, started)
	clock.Phase(protocolcli.OrchestrationPhaseWorkspaceLoad, started)
	snapshot := clock.Snapshot()
	if len(snapshot.Phases) != 2 || snapshot.Phases[0].Phase != protocolcli.OrchestrationPhaseWorkspaceLoad ||
		snapshot.Phases[0].Wall < 10*time.Millisecond {
		t.Fatalf("phases = %+v, want workspace-load summed over its two entries", snapshot.Phases)
	}
	if len(snapshot.Engine.Kinds) != len(procstat.Kinds) {
		t.Errorf("engine ledger = %+v, want every kind", snapshot.Engine)
	}
	if _, measured := procstat.SelfCPU(); measured != snapshot.CPUMeasured {
		t.Errorf("CPUMeasured = %t, the platform says %t", snapshot.CPUMeasured, measured)
	}
	if snapshot.CPUMeasured && snapshot.RunCPU < snapshot.EngineCPU {
		t.Errorf("run cpu %s below engine cpu %s", snapshot.RunCPU, snapshot.EngineCPU)
	}
}

func TestTaskWaitsLedger_StampsWhatATaskEnteredOnce(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cost", "the-session-and-the-result-carry-the-engine-phases-and-the-task-waits")
	job := &ScheduledJob{Project: &workspace.Project{ID: "/site", Name: "site"}, JobDef: &extension.JobDefinition{Name: "build~compile"}}
	var ledger taskWaitsLedger
	started := time.Now().Add(-2 * time.Millisecond)
	ledger.key(job, started)
	ledger.lockWait(job, started)
	ledger.lockWait(job, started)
	result := &JobResult{}
	ledger.stampWaits(job, result)
	if !result.Waits.KeyObserved || !result.Waits.LockObserved || result.Waits.RestoreObserved {
		t.Fatalf("waits = %+v, want key and lock observed, restore not", result.Waits)
	}
	if result.Waits.LockWait < 4*time.Millisecond {
		t.Errorf("lock wait = %s, want both waits summed", result.Waits.LockWait)
	}
	again := &JobResult{}
	ledger.stampWaits(job, again)
	if again.Waits.Any() {
		t.Error("a second result of the same task was stamped again")
	}
	ledger.stampWaits(job, nil)
}
