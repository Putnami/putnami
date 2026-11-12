package output

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A delegated tool writes its own log before it fails, and the job's error is
// the tail of that output. The failure recap quotes the line that names the
// failure, not the tool's first log line.
func TestFailureRecapQuotesTheFailingLineOfADelegatedTool(t *testing.T) {
	cases := map[string]struct {
		output string
		want   string
	}{
		"pants": {
			output: "21:31:22.03 [INFO] Initialization options changed: reinitializing scheduler...\n" +
				"21:31:25.10 [INFO] Scheduler initialized.\n" +
				"21:31:40.52 [ERROR] 1 Exception encountered:\n" +
				"  ServiceMissingError: mongodb is not reachable on localhost:27017\n",
			want: "21:31:40.52 [ERROR] 1 Exception encountered:",
		},
		"rush": {
			output: "@coze-workflow/test-run (lint) completed successfully in 3.2 seconds\n" +
				"@coze-arch/bot-api (lint) completed successfully in 1.1 seconds\n" +
				"FAILURE: @coze-workflow/playground\n" +
				"rush lint (34.51 seconds)\n",
			want: "FAILURE: @coze-workflow/playground",
		},
		"turborepo": {
			output: "@acme/db:lint: cache miss, executing 4b1c\n" +
				"@acme/nextjs:build: > next build\n" +
				"@acme/nextjs:build: ❌ Invalid environment variables: { AUTH_SECRET: [ 'Required' ] }\n" +
				"@acme/nextjs:build: Error: Invalid environment variables\n" +
				" Tasks:    3 successful, 4 total\n",
			want: "@acme/nextjs:build: Error: Invalid environment variables",
		},
		"no line names the failure": {
			output: "[INFO] starting\nsomething went sideways\n",
			want:   "something went sideways",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var errOut bytes.Buffer
			renderer := NewTextRenderer(&bytes.Buffer{}, &errOut, TextRendererConfig{})
			failed := &scheduler.ScheduledJob{
				Project:   &workspace.Project{ID: "/tool", Name: "tool", Path: "tool"},
				Extension: &extension.ExtensionDescription{Name: "@putnami/pants", Path: "/ext"},
				JobDef:    &extension.JobDefinition{Name: "test~test", ExtensionName: "@putnami/pants"},
			}
			passed := makeRendererTestJob()
			renderer.Start([]*scheduler.ScheduledJob{failed, passed})
			results := map[string]*scheduler.JobResult{
				failed.Key(): {Status: string(scheduler.TaskStatusFailed), Error: &scheduler.JobError{Message: strings.TrimSpace(tc.output)}},
				passed.Key(): {Status: string(scheduler.TaskStatusSuccess)},
			}
			renderer.Finish(results, scheduler.SessionOutcome{})

			recap := recapLine(errOut.String(), failed.Key())
			if recap != failed.Key()+" — "+tc.want {
				t.Fatalf("recap line = %q, want %q", recap, failed.Key()+" — "+tc.want)
			}
		})
	}
}

// recapLine is the line of the "Failed:" recap that names key.
func recapLine(text, key string) string {
	_, recap, ok := strings.Cut(text, "Failed:\n")
	if !ok {
		return ""
	}
	for _, line := range strings.Split(recap, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, key) {
			return line
		}
	}
	return ""
}
