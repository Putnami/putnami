package output

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The package's shared renderer-test helpers. They lived in jsonl_test.go until
// deleting that file with the v1 JSONL renderer; the surviving cloud-logging
// and text tests use them, so they moved here rather than into whichever file
// happened to outlive the others.

// makeTestJob builds the minimal scheduled job a renderer needs to name a task.
func makeTestJob(projectName, jobName string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project: &workspace.Project{Name: projectName},
		JobDef:  &extension.JobDefinition{Name: jobName},
	}
}

// parseJSONLines decodes a line-delimited JSON stream, failing the test on the
// first line that is not an object.
func parseJSONLines(t *testing.T, output string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse JSONL line: %v (line: %q)", err, line)
		}
		events = append(events, event)
	}
	return events
}
