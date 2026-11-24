package jobs

import (
	"testing"
	"time"
)

func TestTaskWaits_MergeKeepsWhatWasObserved(t *testing.T) {
	var waits TaskWaits
	if waits.Any() {
		t.Fatal("empty waits observed a step")
	}
	var other TaskWaits
	other.AddRestore(0)
	other.AddKey(2 * time.Millisecond)
	waits.AddKey(time.Millisecond)
	waits.Merge(other)
	if !waits.KeyObserved || waits.Key != 3*time.Millisecond || !waits.RestoreObserved || waits.LockObserved {
		t.Fatalf("waits = %+v", waits)
	}
}

func TestOrchestrationSnapshot_CPUAddsBothOriginsOfCLIAndGit(t *testing.T) {
	snapshot := &OrchestrationSnapshot{
		Engine:       ProcessLedger{Kinds: []ProcessCount{{Kind: "control", CPU: 5}, {Kind: "cli", CPU: 1}, {Kind: "git", CPU: 2}, {Kind: "job", CPU: 1000}}},
		Jobs:         ProcessLedger{Kinds: []ProcessCount{{Kind: "cli", CPU: 10}, {Kind: "git", CPU: 20}, {Kind: "other", CPU: 9000}}},
		EngineCPU:    100,
		ExtensionCPU: 50,
	}
	engine, extension, control, cli, git := snapshot.OrchestrationCPU()
	if engine != 100 || extension != 50 || control != 5 || cli != 10 || git != 22 {
		t.Errorf("cpu = %d %d %d %d %d: job and tool CPU is never orchestration", engine, extension, control, cli, git)
	}
	if got := snapshot.Jobs.Of("batch"); got.Kind != "batch" || got.Starts != 0 {
		t.Errorf("an absent kind = %+v", got)
	}
}
