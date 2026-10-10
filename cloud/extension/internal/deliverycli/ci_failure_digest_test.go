package deliverycli

import (
	"fmt"
	"strings"
	"testing"
)

// The failure digest the runner used to print into its log is served on
// the run view now: `view` names each failed task's error, diagnostics
// and last output lines, and states whether the session record arrived, with
// no log read.
func TestCIViewRendersTheFailureDigestWithoutALogRead(t *testing.T) {
	output := make([]string, 15)
	for i := range output {
		output[i] = fmt.Sprintf("line %d", i+1)
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{
			ID: "run-1", Status: "completed", Conclusion: "failure",
			FailedTasks: []CIRunFailedTask{
				{
					Project: "libs/records", Task: "test~test", Error: "exit status 1",
					Diagnostics: []CIRunFailedTaskDiagnostic{{File: "a.go", Line: 3, Message: "boom"}},
					Output:      output,
				},
				{Project: "libs/cli", Task: "lint~lint", DetailOmitted: true},
			},
			FailedTasksOmitted: 4,
			Session: &CIRunSession{
				Complete: true,
				Session:  CIRunSessionPart{Shipped: true, Chunks: 1, Bytes: 2048, Final: true},
				Events:   CIRunSessionPart{Shipped: true, Chunks: 3, Bytes: 3 << 20, Final: true},
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"view", "run-1"})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"Session:    complete (session 1 chunk(s) 2.0KiB, events 3 chunk(s) 3.0MiB)",
		"PROJECT       TASK       ERROR",
		"libs/records  test~test  exit status 1",
		"libs/cli      lint~lint  -",
		"… and 4 more failing task(s) not shown",
		"    a.go:3: boom",
		"    output (last 10 of 15 lines):",
		"      | line 6",
		"      | line 15",
		"    (diagnostics and output left out by the run view's size bound)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view output never showed %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "| line 5\n") || strings.HasSuffix(out, "| line 5") {
		t.Fatalf("view printed more than the last %d output lines:\n%s", ciFailedTaskOutputLines, out)
	}
	asked := false
	for _, req := range fake.requests {
		if strings.HasSuffix(req.Path, "/logs") {
			t.Fatalf("a run whose digest carries detail fetched its logs: %s %s", req.Method, req.Path)
		}
		if req.Path == "/v1/workspaces/ws-acme/runs/run-1" {
			asked = req.Query == "detail=true"
		}
	}
	if !asked {
		t.Fatalf("view never asked for the run's detail (detail=true): %+v", fake.requests)
	}
}

// The detail members are opt-in on the server, because a client generated
// before them refuses them. Only `view` renders them, so only `view` asks.
func TestCIRunReadsOtherThanViewDoNotAskForDetail(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{ID: "run-1", Status: "completed"}},
	}}
	// The timing table is not stubbed: only the run read matters here.
	_, _ = captureCI(t, fake, []string{"timings", "run-1"})
	seen := false
	for _, req := range fake.requests {
		if req.Path == "/v1/workspaces/ws-acme/runs/run-1" {
			seen = true
			if req.Query != "" {
				t.Fatalf("timings read the run with query %q, want none", req.Query)
			}
		}
	}
	if !seen {
		t.Fatalf("timings never read the run: %+v", fake.requests)
	}
}

func TestCISessionLineSaysWhatArrived(t *testing.T) {
	cases := []struct {
		name    string
		session *CIRunSession
		want    string
	}{
		{"no account", nil, ""},
		{"nothing arrived", &CIRunSession{}, "not received"},
		{
			"events never closed",
			&CIRunSession{
				Session: CIRunSessionPart{Shipped: true, Chunks: 1, Bytes: 10, Final: true},
				Events:  CIRunSessionPart{Shipped: true, Chunks: 4, Bytes: 4096},
			},
			"incomplete (session 1 chunk(s) 10B, events 4 chunk(s) 4.0KiB not closed)",
		},
		{
			"session never arrived",
			&CIRunSession{Events: CIRunSessionPart{Shipped: true, Chunks: 1, Bytes: 10, Final: true}},
			"incomplete (session not received, events 1 chunk(s) 10B)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ciSessionLine(tc.session); got != tc.want {
				t.Fatalf("session line = %q, want %q", got, tc.want)
			}
		})
	}
}

// The wire twins keep the server's member names (apis.CIRunFailedTaskResponse,
// apis.CIRunSessionResponse).
func TestCIRunFailureDigestWireNames(t *testing.T) {
	assertExactKeys(t, "failedTask", marshalToDoc(t, CIRunFailedTask{
		Project: "p", Task: "t", Error: "e",
		Diagnostics:   []CIRunFailedTaskDiagnostic{{File: "a.go", Line: 1, Message: "m"}},
		Output:        []string{"o"},
		SharedOutput:  &CIRunFailedTaskShared{Source: "s", Lines: []string{"l"}},
		DetailOmitted: true,
	}), []string{"project", "task", "error", "diagnostics", "output", "sharedOutput", "detailOmitted"})
	assertExactKeys(t, "session", marshalToDoc(t, CIRunSession{Complete: true}), []string{"complete", "session", "events"})
	assertExactKeys(t, "sessionPart", marshalToDoc(t, CIRunSessionPart{Shipped: true}), []string{"shipped", "chunks", "bytes", "final"})
}
