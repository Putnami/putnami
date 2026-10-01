package output_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A failed `runOn: finally` finalizer must read the same on EVERY surface.
//
// The contract is "a runOn: finally step failed … the invocation's outcome is
// unchanged": a finalizer can neither rescue nor condemn the work it tears
// down. That rule used to be applied by handing the SCHEDULER's own reduction a
// filtered result map, but each renderer's Finish and the machine projection
// re-reduce the RAW map, so the exit code said success while the displayed
// summary, the cloud-logging labels and the machine document all counted the
// finalizer Failed — a cross-surface drift this corpus polices.
//
// This test lives in the renderer package deliberately: it is the consumer side
// of the reduction, and reading the counts off the rendered bytes is what proves
// the surfaces agree rather than that one function was called correctly.

// lastJSONObject decodes the final line of a JSONL stream — the session summary
// a cloud-logging Finish writes last.
func lastJSONObject(t *testing.T, stream string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stream), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &object); err != nil {
			t.Fatalf("parse JSONL line %q: %v", lines[i], err)
		}
		return object
	}
	t.Fatal("stream carried no JSON object")
	return nil
}

func finalizerRunFixture(t *testing.T) ([]*jobs.ScheduledJob, map[string]*jobs.JobResult) {
	t.Helper()
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	node := func(stepID, task, runOn string) *jobs.ScheduledJob {
		step := &extension.PipelineStep{ID: stepID, Task: task, RunOn: runOn}
		if runOn == extensionproto.StepRunOnFinally {
			step.Finalizes = &extensionproto.FinalizesRelation{
				Producer: "setup", Consumers: []string{"run"},
			}
		}
		return &jobs.ScheduledJob{
			Project: project,
			Step:    step,
			JobDef: &extension.JobDefinition{
				Name:        extension.StepJobName("test", stepID),
				CommandName: "test",
				StepID:      stepID,
			},
		}
	}

	producer := node("setup", "test-env-up", "")
	consumer := node("run", "test-exec", "")
	finalizer := node("teardown", "test-env-down", extensionproto.StepRunOnFinally)

	results := map[string]*jobs.JobResult{
		producer.Key():  {Status: "success"},
		consumer.Key():  {Status: "success"},
		finalizer.Key(): {Status: "failed", Error: &jobs.JobError{Code: extensionproto.FailureSensitiveFinalizerFailed, Message: "provider unreachable"}},
	}
	return []*jobs.ScheduledJob{producer, consumer, finalizer}, results
}

func TestFailedFinalizerReadsTheSameOnEverySurface(t *testing.T) {
	planned, results := finalizerRunFixture(t)
	outcome := jobs.SessionOutcome{}

	// 1. The canonical reduction every renderer folds through.
	session := jobs.ReduceRun(planned, results, outcome)
	if session.Status.Failed != 0 {
		t.Errorf("reduction Failed = %d; a finalizer must not vote on the verdict", session.Status.Failed)
	}
	if !session.Success() {
		t.Error("a failed finalizer changed the run's verdict")
	}
	if session.BucketTotal() != 2 {
		t.Errorf("bucket total = %d, want the two tasks that vote", session.BucketTotal())
	}

	// 2. The text renderer's summary line.
	var errOut bytes.Buffer
	text := output.NewTextRenderer(&bytes.Buffer{}, &errOut, output.TextRendererConfig{})
	text.Start(planned)
	text.Finish(results, outcome)
	rendered := errOut.String()
	if strings.Contains(rendered, "failed") {
		t.Errorf("text summary reports a failure the exit code does not:\n%s", rendered)
	}
	if !strings.Contains(rendered, "2 succeeded") {
		t.Errorf("text summary = %q, want the two voting tasks", rendered)
	}

	// 3. The cloud-logging labels.
	var cloudOut bytes.Buffer
	cloud := output.NewCloudLoggingRenderer(&cloudOut)
	cloud.Start(planned)
	cloud.Finish(results, outcome)
	entry := lastJSONObject(t, cloudOut.String())
	summary, _ := entry["logging.googleapis.com/labels"].(map[string]any)
	if summary == nil {
		t.Fatalf("cloud-logging summary carried no labels: %v", entry)
	}
	if got := summary["failed"]; got != float64(0) {
		t.Errorf("cloud-logging failed label = %v, want 0", got)
	}
	if got := summary["succeeded"]; got != float64(2) {
		t.Errorf("cloud-logging succeeded label = %v, want 2", got)
	}
	if got := entry["severity"]; got != "INFO" {
		t.Errorf("cloud-logging severity = %v, want INFO for a run whose exit code is success", got)
	}

	// 4. The machine projection.
	run := machine.RunOf(planned, results, outcome)
	if run.Session.Status.Failed != 0 {
		t.Errorf("machine session Failed = %d, want 0", run.Session.Status.Failed)
	}

	// The row is NOT swallowed on any of them: it stays in the raw results, and
	// the machine projection still NAMES it. "Does not vote" is not "is hidden".
	if len(run.Tasks) != 3 {
		t.Errorf("machine task list = %d rows, want the finalizer listed alongside the voters", len(run.Tasks))
	}
	if results[planned[2].Key()].Status != "failed" {
		t.Error("the finalizer's own row lost its failure")
	}
}
