package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The session-event PRODUCER pin.
//
// internal/cli/testdata/wire pins the session WRITER — it feeds payload maps to
// workspace_state.Session and compares the files it leaves on disk. It cannot
// pin emitSessionJobEnd, which is unexported, so it reproduces that method's key
// set instead. That leaves one hole this file closes: change the producer's keys
// and the reader together and those goldens still pass while a real events.jsonl
// changes underneath `putnami sessions inspect`.
//
// So this file asserts the payload directly, at the producer, and asserts that
// the TYPED half of the record says the same thing as the untyped half. A2b
// derives the map from the typed task; without the second assertion, deriving it
// wrongly would be invisible here.

// producerFixtureJob is the job shape emitSessionJobEnd reads: a pipeline step,
// so taskKind and job differ and the payload cannot conflate them.
func producerFixtureJob(cpuBudget int) *ScheduledJob {
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/services/api", Name: "@acme/api"},
		Extension: &extension.ExtensionDescription{Name: "go"},
		JobDef:    &extension.JobDefinition{Name: "build~compile", CommandName: "build"},
		Step:      &extension.PipelineStep{ID: "compile", Task: "build-compile"},
		CPUBudget: cpuBudget,
	}
}

func captureJobEnd(t *testing.T, job *ScheduledJob, result *JobResult) SessionRecord {
	t.Helper()
	var captured SessionRecord
	var count int
	scheduler := &Scheduler{onSessionEvent: func(record SessionRecord) {
		captured = record
		count++
	}}
	scheduler.emitSessionJobEnd(job, result)
	if count != 1 {
		t.Fatalf("emitSessionJobEnd produced %d records, want exactly 1", count)
	}
	return captured
}

// TestJobEndPayload_PinsTheRecordedWire pins the exact key set, value types and
// values of the payload events.jsonl persists, for a fresh task and for a reused
// one. sessions_helpers.go reads "duration", "cache" and "coalesced" off this shape
// and workspace_state projects the rest into session.json's job table.
func TestJobEndPayload_PinsTheRecordedWire(t *testing.T) {
	t.Parallel()
	failure := &JobResult{
		Status:             "failed",
		Duration:           850 * time.Millisecond,
		TaskWall:           902 * time.Millisecond,
		CPUTime:            1500 * time.Millisecond,
		SpawnToFirstEvent:  37 * time.Millisecond,
		FirstEventObserved: true,
		ExitCode:           2,
		Error:              &JobError{Message: "2 assertions failed", Code: "E_TEST"},
		Execution:          &Execution{ID: "exec-7", Wall: 900 * time.Millisecond},
	}
	cached := &JobResult{Status: "success", Duration: 3 * time.Millisecond, TaskWall: 12 * time.Millisecond}
	cached.MarkReuse(ReuseRemoteCache)

	for _, test := range []struct {
		name      string
		result    *JobResult
		cpuBudget int
		want      map[string]any
	}{
		{
			name:      "fresh failure with every optional key",
			result:    failure,
			cpuBudget: 4,
			want: map[string]any{
				"project":             "@acme/api",
				"job":                 "build~compile",
				"taskKind":            "build-compile",
				"extension":           "go",
				"status":              "failed",
				"outcome":             "failed",
				"duration":            int64(850),
				"taskWallMs":          int64(902),
				"cache":               false,
				"coalesced":           false,
				"spawnToFirstEventMs": int64(37),
				"cpuBudget":           4,
				"cpuTimeMs":           int64(1500),
				"executionId":         "exec-7",
				"error":               "2 assertions failed",
			},
		},
		{
			// A remote hit reports "cached", like a local one: the distinction is
			// real but belongs to the cache summary, and changing this string
			// would break sessions written by an older CLI.
			name:   "remote cache hit omits every optional key",
			result: cached,
			want: map[string]any{
				"project":    "@acme/api",
				"job":        "build~compile",
				"taskKind":   "build-compile",
				"extension":  "go",
				"status":     "success",
				"outcome":    "cached",
				"duration":   int64(3),
				"taskWallMs": int64(12),
				"cache":      true,
				"coalesced":  false,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := captureJobEnd(t, producerFixtureJob(test.cpuBudget), test.result)

			if record.Type != SessionRecordJobEnd || record.JobKey != "/services/api:build~compile" {
				t.Fatalf("record identity = %q %q", record.Type, record.JobKey)
			}
			if len(record.Data) != len(test.want) {
				t.Fatalf("payload has %d keys, want %d\n got: %#v\nwant: %#v",
					len(record.Data), len(test.want), record.Data, test.want)
			}
			for key, want := range test.want {
				got, present := record.Data[key]
				if !present {
					t.Errorf("payload is missing %q", key)
					continue
				}
				if got != want {
					t.Errorf("payload[%q] = %#v (%T), want %#v (%T)", key, got, got, want, want)
				}
			}

			// The typed half must agree with the untyped half field for field.
			// A2b derives the map FROM the task, so a wrong derivation is exactly
			// what this catches.
			task := record.Task
			if task == nil {
				t.Fatal("job:end record carries no typed task")
			}
			if task.Key != record.JobKey || task.Project != record.Data["project"] ||
				task.Job != record.Data["job"] || task.TaskKind != record.Data["taskKind"] ||
				task.Extension != record.Data["extension"] {
				t.Errorf("typed identity %+v disagrees with payload %#v", task, record.Data)
			}
			if string(task.Status) != record.Data["status"] || task.Outcome() != record.Data["outcome"] {
				t.Errorf("typed verdict (%q/%q) disagrees with payload (%v/%v)",
					task.Status, task.Outcome(), record.Data["status"], record.Data["outcome"])
			}
			if task.Reuse.CacheHit() != record.Data["cache"] ||
				(task.Reuse == ReuseCoalesced) != record.Data["coalesced"] {
				t.Errorf("typed reuse %q disagrees with payload cache=%v coalesced=%v",
					task.Reuse, record.Data["cache"], record.Data["coalesced"])
			}
			if task.Timing.Duration.Milliseconds() != record.Data["duration"] ||
				task.Timing.TaskWall.Milliseconds() != record.Data["taskWallMs"] {
				t.Errorf("typed timings %+v disagree with payload %#v", task.Timing, record.Data)
			}

			// The payload has to survive a JSON round trip as the shape the gate
			// decodes: numbers become float64 there, so an unmarshalable value
			// (a time.Duration, a typed status) would be caught here and not in
			// production three releases later.
			encoded, err := json.Marshal(record.Data)
			if err != nil {
				t.Fatalf("payload does not marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("payload does not round-trip: %v", err)
			}
			if len(decoded) != len(test.want) {
				t.Errorf("round-tripped payload has %d keys, want %d: %s", len(decoded), len(test.want), encoded)
			}
		})
	}
}

// TestJobEndPayload_StatusIsTheRawJobResultStatus pins the one place the typed
// projection could legitimately alter a persisted string. TaskResultOf maps
// alias spellings ("OK", "failure", "skip") onto the canonical vocabulary, so a
// JobResult carrying an alias would be recorded normalized. Every ingress
// normalizes already — the event path through normalizeStatus, the batch path
// through parseTaskStatus, and the scheduler's own literals — which is what
// makes deriving the payload from the task byte-identical. This asserts that
// premise instead of trusting it.
func TestJobEndPayload_StatusIsTheRawJobResultStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"success", "failed", "canceled", "skipped", "mystery"} {
		record := captureJobEnd(t, producerFixtureJob(0), &JobResult{Status: status})
		if got := record.Data["status"]; got != status {
			t.Errorf("JobResult status %q recorded as %v", status, got)
		}
		if got := normalizeStatus(status); got != status {
			t.Errorf("normalizeStatus(%q) = %q: an ingress can produce a status the "+
				"producer would rewrite, so the payload is no longer byte-identical", status, got)
		}
	}
}

// TestEmitSessionJobEnd_DeDuplicatesPerJob keeps the terminal record at most
// once per job key across the coordinator paths that can all reach it (normal
// completion, drain, stuck DAG, cancellation synthesis). A duplicate job:end
// record would list the task twice in `putnami sessions inspect`.
func TestEmitSessionJobEnd_DeDuplicatesPerJob(t *testing.T) {
	t.Parallel()
	var records []SessionRecord
	scheduler := &Scheduler{onSessionEvent: func(record SessionRecord) {
		records = append(records, record)
	}}
	job := producerFixtureJob(0)
	for i := 0; i < 3; i++ {
		scheduler.emitSessionJobEnd(job, &JobResult{Status: "success"})
	}
	if len(records) != 1 {
		t.Fatalf("emitted %d job:end records for one job, want 1", len(records))
	}
}
