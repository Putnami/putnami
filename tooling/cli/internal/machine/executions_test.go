package machine

import (
	"encoding/json"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The physical execution ledger's projection.
//
// The run view holds LOGICAL tasks; the session file must additionally carry
// the PHYSICAL executions those tasks came out of, each listed once. These
// tests pin the two directions that make a cost roll-up trustworthy: the
// ledger counts a shared subprocess once however many task records rode it,
// and every record that names an execution names one the document declares.

// batchedRun is one batch of three projects sharing a single subprocess, one
// project that ran alone, and one served from cache. It is the shape the epic's
// baseline measured: five logical rows over two physical executions.
func batchedRun() Run {
	shared := &jobs.Execution{
		ID:          "exec-1",
		Wall:        3 * time.Second,
		UserCPU:     9 * time.Second,
		SystemCPU:   2 * time.Second,
		MaxRSSBytes: 700 << 20,
		IOOutBlocks: 128,
		Concurrency: 4,
	}
	solo := &jobs.Execution{
		ID:        "exec-2",
		Wall:      time.Second,
		UserCPU:   900 * time.Millisecond,
		SystemCPU: 100 * time.Millisecond,
	}
	member := func(key string, execution *jobs.Execution, reuse jobs.ReuseKind) Task {
		return Task{
			Identity: identityOfKey(key),
			Result: jobs.TaskResult{
				Status:    jobs.TaskStatusSuccess,
				Reuse:     reuse,
				Execution: execution,
				Timing:    jobs.TaskTiming{Duration: time.Second},
			},
		}
	}
	return Run{
		Session: &jobs.SessionResult{
			Tasks:  5,
			Status: jobs.TaskCounts{Succeeded: 5},
			Reuse:  jobs.ReuseCounts{LocalCache: 1},
			// The scheduler's accumulated ledger, which is what the projection
			// reads — not the task list.
			Executions: []jobs.Execution{*shared, *solo},
		},
		Tasks: []Task{
			member("/a:lint", shared, jobs.ReuseNone),
			member("/b:lint", shared, jobs.ReuseNone),
			member("/c:lint", shared, jobs.ReuseNone),
			member("/d:lint", solo, jobs.ReuseNone),
			member("/e:lint", nil, jobs.ReuseLocalCache),
		},
	}
}

// TestExecutions_CountPhysicalWorkOnce is the property the epic's arithmetic
// rests on: five logical rows, two physical executions, and the CPU total is
// the machine's, not the fan-out's.
func TestExecutions_CountPhysicalWorkOnce(t *testing.T) {
	executions := batchedRun().Executions()
	if len(executions) != 2 {
		t.Fatalf("ledger lists %d executions, want 2 for five logical rows", len(executions))
	}

	shared := executions[0]
	if shared.ID != "exec-1" {
		t.Errorf("ledger order = %q first, want first appearance in the reduction", shared.ID)
	}
	if shared.Tasks != 3 {
		t.Errorf("shared execution reports %d tasks, want the 3 records that reference it", shared.Tasks)
	}
	if shared.WallMs != 3000 || shared.UserCPUMs != 9000 || shared.SystemCPUMs != 2000 {
		t.Errorf("shared execution timings = %dms wall / %dms user / %dms system",
			shared.WallMs, shared.UserCPUMs, shared.SystemCPUMs)
	}
	if shared.MaxRSSBytes != 700<<20 || shared.IOOutBlocks != 128 || shared.Concurrency != 4 {
		t.Errorf("shared execution rusage = %d bytes / %d out-blocks / %d concurrency",
			shared.MaxRSSBytes, shared.IOOutBlocks, shared.Concurrency)
	}
	// An unread counter is ABSENT, never a measured zero: omitempty is what
	// carries that distinction onto the wire, so the projection must not invent
	// a value for it.
	if shared.IOInBlocks != 0 {
		t.Errorf("ioInBlocks = %d, want the unmeasured 0", shared.IOInBlocks)
	}

	var physicalCPUMs, logicalCPUMs int64
	for _, execution := range executions {
		physicalCPUMs += execution.UserCPUMs + execution.SystemCPUMs
	}
	for _, task := range batchedRun().Tasks {
		logicalCPUMs += task.Result.Execution.CPUTime().Milliseconds()
	}
	if physicalCPUMs != 12000 {
		t.Errorf("physical CPU = %dms, want 12000", physicalCPUMs)
	}
	if logicalCPUMs <= physicalCPUMs {
		t.Fatalf("the fixture does not fan out: logical %dms vs physical %dms", logicalCPUMs, physicalCPUMs)
	}
}

// TestExecutions_ReuseSpendsNothingPhysical: a cache hit contributes no
// execution and its record names none, which is how a consumer separates work
// from reuse without reading a duration.
func TestExecutions_ReuseSpendsNothingPhysical(t *testing.T) {
	run := batchedRun()
	records := run.Records()
	if got := records[4].ExecutionID; got != "" {
		t.Errorf("cached record references execution %q, want none", got)
	}
	for i := range 3 {
		if records[i].ExecutionID != "exec-1" {
			t.Errorf("batch member %d references %q, want the shared exec-1", i, records[i].ExecutionID)
		}
	}
	if records[3].ExecutionID != "exec-2" {
		t.Errorf("solo record references %q, want exec-2", records[3].ExecutionID)
	}
}

// TestSessionFile_LedgerValidatesAgainstTheContract runs the projection through
// the protocol's own validator, so the referential rule (a record's executionId
// must name a declared execution) is proven on the document this CLI actually
// writes rather than on a hand-built one.
func TestSessionFile_LedgerValidatesAgainstTheContract(t *testing.T) {
	file := batchedRun().SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "20260805-101500-abc123"
	file.StartTime = "2026-08-05T10:15:00.000Z"

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session file violates the contract: %v\n%s", violations, data)
	}

	declared := make(map[string]int, len(file.Executions))
	for _, execution := range file.Executions {
		declared[execution.ID] = execution.Tasks
	}
	referenced := make(map[string]int, len(file.Executions))
	for i, task := range file.Tasks {
		if task.ExecutionID == "" {
			continue
		}
		if _, ok := declared[task.ExecutionID]; !ok {
			t.Errorf("tasks[%d].executionId = %q has no matching executions[] entry", i, task.ExecutionID)
			continue
		}
		referenced[task.ExecutionID]++
	}
	for id, want := range declared {
		if referenced[id] != want {
			t.Errorf("execution %q declares %d tasks but %d records reference it", id, want, referenced[id])
		}
	}
}

// TestExecutions_KeepASupersededRetryAttempt is the under-count regression: a
// retried task's result holds only its FINAL execution, so a ledger derived
// from the task list would lose attempt 1's wall, CPU and peak RSS entirely.
// The scheduler's accumulated ledger keeps it, referenced by nobody, with
// tasks 0 saying exactly that.
func TestExecutions_KeepASupersededRetryAttempt(t *testing.T) {
	first := jobs.Execution{ID: "exec-1", Wall: 4 * time.Second, UserCPU: 7 * time.Second}
	retry := jobs.Execution{ID: "exec-2", Wall: 2 * time.Second, UserCPU: 3 * time.Second}
	run := Run{
		Session: &jobs.SessionResult{
			Tasks:  1,
			Status: jobs.TaskCounts{Succeeded: 1},
			// Both attempts ran; only the second survived onto the task.
			Executions: []jobs.Execution{first, retry},
		},
		Tasks: []Task{{
			Identity: identityOfKey("/a:lint"),
			Result:   jobs.TaskResult{Status: jobs.TaskStatusSuccess, Execution: &retry},
		}},
	}

	executions := run.Executions()
	if len(executions) != 2 {
		t.Fatalf("ledger lists %d executions, want both attempts", len(executions))
	}
	if executions[0].ID != "exec-1" || executions[0].Tasks != 0 {
		t.Errorf("superseded attempt = %+v, want exec-1 with tasks 0", executions[0])
	}
	if executions[1].ID != "exec-2" || executions[1].Tasks != 1 {
		t.Errorf("final attempt = %+v, want exec-2 with tasks 1", executions[1])
	}

	var cpuMs int64
	for _, execution := range executions {
		cpuMs += execution.UserCPUMs + execution.SystemCPUMs
	}
	if cpuMs != 10000 {
		t.Errorf("physical CPU = %dms, want 10000 — the retried attempt's 7s is real work", cpuMs)
	}
	if records := run.Records(); records[0].ExecutionID != "exec-2" {
		t.Errorf("task references %q, want its final attempt exec-2", records[0].ExecutionID)
	}
}

// TestExecutions_KeepABatchLeaderEveryMemberSuperseded is the worse half of the
// same bug: when a batch split cannot be attributed, every member re-executes
// solo, so NO surviving result names the leader. A task-derived ledger would
// not emit that multi-second subprocess at all.
func TestExecutions_KeepABatchLeaderEveryMemberSuperseded(t *testing.T) {
	leader := jobs.Execution{ID: "exec-1", Wall: 9 * time.Second, UserCPU: 20 * time.Second, MaxRSSBytes: 900 << 20}
	soloA := jobs.Execution{ID: "exec-2", Wall: 3 * time.Second, UserCPU: 5 * time.Second}
	soloB := jobs.Execution{ID: "exec-3", Wall: 3 * time.Second, UserCPU: 5 * time.Second}
	run := Run{
		Session: &jobs.SessionResult{
			Tasks:      2,
			Status:     jobs.TaskCounts{Succeeded: 2},
			Executions: []jobs.Execution{leader, soloA, soloB},
		},
		Tasks: []Task{
			{Identity: identityOfKey("/a:lint"), Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Execution: &soloA}},
			{Identity: identityOfKey("/b:lint"), Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess, Execution: &soloB}},
		},
	}

	executions := run.Executions()
	if len(executions) != 3 {
		t.Fatalf("ledger lists %d executions, want the leader plus both solo retries", len(executions))
	}
	if executions[0].ID != "exec-1" || executions[0].Tasks != 0 {
		t.Fatalf("unreferenced batch leader = %+v, want exec-1 with tasks 0", executions[0])
	}
	if executions[0].UserCPUMs != 20000 || executions[0].MaxRSSBytes != 900<<20 {
		t.Errorf("leader lost its measurement: %+v", executions[0])
	}

	// The document must still validate: the contract permits an execution
	// nothing references precisely so this can be reported.
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "s"
	file.StartTime = "t"
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("session file with an unreferenced execution violates the contract: %v", violations)
	}
}

// TestSessionFile_OmitsAnEmptyLedger keeps the change additive: an all-cached
// run writes exactly the document it wrote before this slice.
func TestSessionFile_OmitsAnEmptyLedger(t *testing.T) {
	run := Run{
		Session: &jobs.SessionResult{Tasks: 1, Status: jobs.TaskCounts{Succeeded: 1}},
		Tasks: []Task{{
			Identity: identityOfKey("/a:lint"),
			Result:   jobs.TaskResult{Status: jobs.TaskStatusSuccess, Reuse: jobs.ReuseLocalCache},
		}},
	}
	if executions := run.Executions(); executions != nil {
		t.Errorf("all-cached run listed %d executions", len(executions))
	}
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "s"
	file.StartTime = "t"
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	for _, member := range []string{"executions", "executionId"} {
		if bytesHave(data, member) {
			t.Errorf("empty ledger emitted %q: %s", member, data)
		}
	}
}

func bytesHave(data []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
