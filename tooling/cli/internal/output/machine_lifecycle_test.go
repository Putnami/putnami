package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// A run of 500 tasks spends the ordinary partition (767 records) long before
// its last task starts. Every task still gets its task:start and its task:end
// on the live stream: the budget bounds the tasks' detail, never the record
// that a task ran.
func TestMachineStreamCarriesTheLifecycleOfEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-machine-detail", "every-task-streams-its-start-and-its-end")
	const tasks = 500
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(newMachineV2Renderer(&live, "lint", true), session, false)

	planned := make([]*scheduler.ScheduledJob, tasks)
	for i := range planned {
		planned[i] = &scheduler.ScheduledJob{
			Project:   &workspace.Project{ID: fmt.Sprintf("/p%03d", i), Name: fmt.Sprintf("p%03d", i), Path: fmt.Sprintf("p%03d", i)},
			Extension: &extension.ExtensionDescription{Name: "test-ext", Path: "/ext"},
			JobDef:    &extension.JobDefinition{Name: "lint", ExtensionName: "test-ext"},
		}
	}
	results := make(map[string]*scheduler.JobResult, tasks)
	renderer.Start(planned)
	for i, job := range planned {
		renderer.JobStart(job)
		for range 3 {
			event := logEvent("info", "checked "+job.Project.Name)
			event.Version = 2
			renderer.JobEvent(job, event)
		}
		result := &scheduler.JobResult{Status: string(scheduler.TaskStatusSuccess), Duration: time.Millisecond}
		if i == tasks-1 {
			result = &scheduler.JobResult{Status: string(scheduler.TaskStatusFailed), Error: &scheduler.JobError{Message: "lint failed"}}
		}
		results[job.Key()] = result
		renderer.JobComplete(job, result)
	}
	renderer.Finish(results, scheduler.SessionOutcome{})
	session.Close()

	starts, ends := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'}) {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		switch record.Record {
		case protocolcli.RecordTaskStart:
			starts++
		case protocolcli.RecordTaskEnd:
			ends++
		}
	}
	if starts != tasks || ends != tasks {
		t.Fatalf("live stream holds %d task:start and %d task:end, want %d of each", starts, ends, tasks)
	}

	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("bounded stream violations: %+v", violations)
	}
	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
	var final protocolcli.BoundedSessionEndRecord
	if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.MachineOutput.Elided.Ordinary.Records == 0 {
		t.Fatal("the run's detail did not spend the ordinary partition: the test proves nothing")
	}
	if !strings.Contains(string(lines[len(lines)-2]), "lint failed") {
		t.Fatalf("the last task's task:end lost its error: %s", lines[len(lines)-2])
	}
}
