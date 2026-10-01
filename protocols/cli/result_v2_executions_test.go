package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The physical execution ledger.
//
// The task list is logical and fans out; the execution list is physical and is
// listed once. These tests pin the two properties a later saving claim is
// judged against: a session file carrying executions round-trips through the Go
// types without losing a member, and a document whose task references an
// execution nobody declared is rejected rather than quietly dropping that
// task's cost from the physical roll-up.

func sessionFileWithExecutions() SessionFile {
	identity := func(projectID, name string) TaskIdentity {
		return TaskIdentity{
			Key:     projectID + ":lint",
			Scope:   TaskScopeProject,
			Project: ProjectIdentity{ID: projectID, Name: name},
			Task:    TaskRef{Name: "lint", Command: "lint", Kind: "go-lint"},
			Provider: ProviderIdentity{
				Extension: "@putnami/go",
			},
		}
	}
	return SessionFile{
		ProtocolVersion: ResultProtocolVersion,
		SessionID:       "20260805-101500-abc123",
		StartTime:       "2026-08-05T10:15:00.000Z",
		EndTime:         "2026-08-05T10:15:03.000Z",
		Commands:        []string{"lint"},
		Run: RunSummary{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{Total: 2, Succeeded: 2},
			DurationMs: 3000,
		},
		Tasks: []TaskRecord{
			{
				Identity:    identity("/tooling/cli", "@putnami/cli"),
				ExecutionID: "exec-1",
				Status:      TaskStatusSuccess,
				Reuse:       TaskReuseNone,
				DurationMs:  1500,
			},
			{
				Identity:    identity("/protocols/cli", "@putnami/cli-protocol-go"),
				ExecutionID: "exec-1",
				Status:      TaskStatusSuccess,
				Reuse:       TaskReuseNone,
				DurationMs:  1500,
			},
		},
		Executions: []ExecutionRecord{
			{
				ID:          "exec-1",
				WallMs:      3000,
				UserCPUMs:   4200,
				SystemCPUMs: 610,
				MaxRSSBytes: 734003200,
				IOOutBlocks: 128,
				Concurrency: 4,
				Tasks:       2,
			},
			// A superseded attempt: real work, referenced by nothing.
			{
				ID:          "exec-2",
				WallMs:      410,
				UserCPUMs:   380,
				SystemCPUMs: 45,
				Concurrency: 4,
				Tasks:       0,
			},
		},
	}
}

// TestSessionFileExecutionsRoundTrip pins the ledger through the wire: every
// member survives marshal/unmarshal, and the resulting document validates.
func TestSessionFileExecutionsRoundTrip(t *testing.T) {
	want := sessionFileWithExecutions()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SessionFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost a member:\n got: %+v\nwant: %+v", got, want)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionFileExecutionsAreOptional keeps the change ADDITIVE: a session
// file written before the ledger existed — no executions, no executionId —
// still validates unchanged.
func TestSessionFileExecutionsAreOptional(t *testing.T) {
	file := sessionFileWithExecutions()
	file.Executions = nil
	for i := range file.Tasks {
		file.Tasks[i].ExecutionID = ""
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytesContain(data, "executions") || bytesContain(data, "executionId") {
		t.Errorf("empty ledger still emits a member: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionFileAcceptsASupersededExecution is the converse of the referential
// rule, and it is deliberate: an execution nothing references is a retry
// attempt that ran, so the contract must let a producer report it. Dropping it
// would be the under-count the ledger exists to prevent.
func TestSessionFileAcceptsASupersededExecution(t *testing.T) {
	file := sessionFileWithExecutions()
	if file.Executions[1].Tasks != 0 {
		t.Fatalf("fixture's superseded execution reports %d tasks, want 0", file.Executions[1].Tasks)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none for an unreferenced execution", violations)
	}
}

// TestSessionFileRejectsDanglingExecutionID is the referential rule: a record
// may reference no execution, but never one the document does not declare.
func TestSessionFileRejectsDanglingExecutionID(t *testing.T) {
	file := sessionFileWithExecutions()
	file.Tasks[1].ExecutionID = "exec-9"
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []Violation{{Code: ViolationInvalidKey, Path: "tasks[1].executionId"}}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestSessionFileExecutionsAggregateOnce is the property the epic's savings
// arithmetic rests on: the physical roll-up reads the execution list, so it is
// invariant under logical fan-out, while the task list still attributes to
// every project.
func TestSessionFileExecutionsAggregateOnce(t *testing.T) {
	file := sessionFileWithExecutions()

	// 4200+610 for the batch, plus 380+45 for the superseded attempt. The
	// superseded execution is IN the total on purpose: the machine ran it.
	var physicalCPUMs int64
	for _, execution := range file.Executions {
		physicalCPUMs += execution.UserCPUMs + execution.SystemCPUMs
	}
	if want := int64(5235); physicalCPUMs != want {
		t.Errorf("physical CPU = %dms, want %dms", physicalCPUMs, want)
	}

	projects := make(map[string]bool)
	for _, task := range file.Tasks {
		projects[task.Identity.Project.ID] = true
	}
	if len(projects) != 2 {
		t.Errorf("logical attribution covers %d projects, want 2", len(projects))
	}

	// Adding a third member to the same execution changes the logical fan-out
	// and must not change the physical total.
	file.Tasks = append(file.Tasks, TaskRecord{
		Identity:    file.Tasks[0].Identity,
		ExecutionID: "exec-1",
		Status:      TaskStatusSuccess,
		Reuse:       TaskReuseNone,
		DurationMs:  1000,
	})
	file.Executions[0].Tasks = 3
	var after int64
	for _, execution := range file.Executions {
		after += execution.UserCPUMs + execution.SystemCPUMs
	}
	if after != physicalCPUMs {
		t.Errorf("physical CPU moved with logical fan-out: %dms → %dms", physicalCPUMs, after)
	}
}

func bytesContain(data []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
