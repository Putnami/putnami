package test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// resultError runs the solo job, emits its result the way the SDK's cli.Run
// does, and returns the status and the result event's error member (nil when
// the event carries none).
func resultError(t *testing.T, ctx *pctx.Context) (string, map[string]any) {
	t.Helper()
	var status string
	var runErr error
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		var data map[string]any
		status, data, runErr = Run(ctx, emit, nil)
		emit.Result(status, data)
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	results := eventsOfType(events, "result")
	if len(results) != 1 {
		t.Fatalf("result events = %d, want 1; events = %+v", len(results), events)
	}
	data, _ := results[0]["data"].(map[string]any)
	jobErr, _ := data["error"].(map[string]any)
	return status, jobErr
}

// TestRun_ATimedOutTestBinaryMarksItsResult runs the real toolchain: a test
// that outlives -timeout makes `go test` print its timeout panic, and the
// FAILED result then carries error.timedOut, so the CLI never replays it.
func TestRun_ATimedOutTestBinaryMarksItsResult(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module slowmod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "slowmod.go"), "package slowmod\n\nfunc Value() int { return 1 }\n")
	mustWrite(t, filepath.Join(dir, "slowmod_test.go"), strings.Join([]string{
		"package slowmod",
		"",
		"import (",
		"\t\"testing\"",
		"\t\"time\"",
		")",
		"",
		"func TestSlow(t *testing.T) {",
		"\ttime.Sleep(time.Hour)",
		"\t_ = Value()",
		"}",
		"",
	}, "\n"))
	params := pctx.Params{
		"timeout":  json.RawMessage(`"1s"`),
		"coverage": json.RawMessage(`false`),
	}
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "slowmod", FullPath: dir},
		Params:        params,
	}

	status, jobErr := resultError(t, ctx)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if jobErr["timedOut"] != true {
		t.Fatalf("result error = %v, want timedOut true", jobErr)
	}
}

// TestRun_AnOrdinaryFailureLeavesItsResultUnmarked is the control: a failed
// run with no timeout panic emits no timedOut mark, so the CLI may replay it.
func TestRun_AnOrdinaryFailureLeavesItsResultUnmarked(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/"+filepath.Base(dir)+"\n\ngo 1.21\n")
	output := strings.Join([]string{
		testEvent("run", "example.com/x", "TestOne", ""),
		testEvent("output", "example.com/x", "TestOne", "    one_test.go:9: panic: test timed out after 1s\n"),
		testEvent("fail", "example.com/x", "TestOne", ""),
		testEvent("fail", "example.com/x", "", ""),
	}, "\n") + "\n"
	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte(output), errors.New("exit status 1")
	})
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "x", FullPath: dir},
		Params:        pctx.Params{},
	}

	status, jobErr := resultError(t, ctx)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if jobErr != nil {
		t.Fatalf("result error = %v, want none for an ordinary failure", jobErr)
	}
}

// TestRunBatchMarksOnlyTheMembersATimeoutFailed pins the batch form: a member
// carries timedOut when its own output, or the unattributed failure charged
// to it, shows a test binary past -timeout, and never otherwise.
func TestRunBatchMarksOnlyTheMembersATimeoutFailed(t *testing.T) {
	cases := []struct {
		name   string
		output []string
		want   map[string]bool
	}{
		{
			name: "a member's own package timed out",
			output: []string{
				testEvent("run", "example.com/a", "TestSlow", ""),
				testEvent("output", "example.com/a", "TestSlow", "panic: test timed out after 1s\n"),
				testEvent("output", "example.com/a", "TestSlow", "\trunning tests:\n"),
				testEvent("fail", "example.com/a", "TestSlow", ""),
				testEvent("fail", "example.com/a", "", ""),
				testEvent("run", "example.com/b", "TestB", ""),
				testEvent("output", "example.com/b", "TestB", "    b_test.go:9: boom\n"),
				testEvent("fail", "example.com/b", "TestB", ""),
				testEvent("fail", "example.com/b", "", ""),
			},
			want: map[string]bool{"/a": true, "/b": false},
		},
		{
			name: "an unattributed package timed out",
			output: []string{
				testEvent("output", "example.com/a/nested", "TestSlow", "panic: test timed out after 1s\n"),
				testEvent("fail", "example.com/a/nested", "", ""),
				testEvent("pass", "example.com/b", "", ""),
			},
			want: map[string]bool{"/a": true, "/b": true},
		},
		{
			name: "no package timed out",
			output: []string{
				testEvent("output", "example.com/a", "TestA", "    a_test.go:3: boom\n"),
				testEvent("fail", "example.com/a", "TestA", ""),
				testEvent("fail", "example.com/a", "", ""),
				testEvent("pass", "example.com/b", "", ""),
			},
			want: map[string]bool{"/a": false, "/b": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := makeBatchTestContext(t, false)
			output := strings.Join(tc.output, "\n") + "\n"
			mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
				return []byte(output), errors.New("exit status 1")
			})

			var data map[string]any
			var runErr error
			captureJobEvents(t, func(emit *jsonl.Emitter) {
				_, data, runErr = runBatch(ctx, emit)
			})
			if runErr != nil {
				t.Fatalf("runBatch: %v", runErr)
			}
			for _, result := range data["batchResults"].([]batchProjectResult) {
				want := tc.want[result.ProjectID]
				if result.TimedOut != want {
					t.Errorf("%s TimedOut = %v, want %v (status %s)", result.ProjectID, result.TimedOut, want, result.Status)
				}
				if want && result.Status != "FAILED" {
					t.Errorf("%s is marked timed out with status %s, want FAILED", result.ProjectID, result.Status)
				}
				wire, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(string(wire), `"timedOut":true`); got != want {
					t.Errorf("%s wire = %s, want timedOut member %v", result.ProjectID, wire, want)
				}
			}
		})
	}
}
