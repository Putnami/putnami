package deliverycli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func ciResultsFake(results ciStubResponse) *ciFakeServer {
	return &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{
			ID: "run-9", Status: "completed", Conclusion: "failure",
		}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights/results": results,
	}}
}

// The findings come first: the failed case with its output, the coverage row
// under its threshold, the diagnostic at its file and line, and every bound the
// server applied is stated.
func TestCIResultsRendersWhatTheGateFound(t *testing.T) {
	output := make([]string, 25)
	for i := range output {
		output[i] = "line " + string(rune('a'+i))
	}
	fake := ciResultsFake(ciStubResponse{status: 200, body: map[string]any{
		"runId": "run-9", "sessionId": "20260912-101112-abcdef",
		"tests": []map[string]any{{
			"key": "/go/libs/a:test~test", "project": "/go/libs/a", "task": "test~test", "status": "failed",
			"passed": 41, "failed": 1, "skipped": 2, "dropped": 3, "omitted": 4,
			"cases": []map[string]any{
				{"name": "TestParse", "suite": "example.com/a", "status": "failed", "durationMs": 12,
					"output": strings.Join(output, "\n"), "file": "go/libs/a/parse_test.go", "line": 41},
				{"name": "TestOK", "suite": "example.com/a", "status": "passed", "durationMs": 1},
			},
		}},
		"elidedTestTasks": 5, "omittedCases": 4,
		"coverage": []map[string]any{{
			"key": "/go/libs/a:test~test", "project": "/go/libs/a", "task": "test~test",
			"percentage": 65.5, "granularity": "statements", "covered": 655, "total": 1000,
			"threshold": 70, "belowThreshold": true,
		}},
		"diagnostics": []map[string]any{{
			"key": "/go/libs/a:lint~vet", "project": "/go/libs/a", "task": "lint~vet", "status": "failed",
			"errors": 1, "warnings": 0, "infos": 0, "omitted": 6,
			"items": []map[string]any{{"severity": "error", "code": "SA4006", "file": "go/libs/a/x.go", "line": 7, "column": 3, "message": "value never used"}},
		}},
		"publications": []map[string]any{{
			"key": "/svc:publish~docker", "project": "/svc", "task": "publish~docker", "kind": "published",
			"registry": "docker", "name": "oci.putnami.dev/putnami/svc", "version": "1.0.0",
			"digest": "sha256:abc", "tags": []string{"canary"},
		}},
		"malformedLines": 2,
	}})
	lines, err := captureCI(t, fake, []string{"results", "run-9"})
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/runs/run-9/insights/results" {
		t.Fatalf("request = %s %s, want GET the run results route", got.Method, got.Path)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"20260912-101112-abcdef",
		"2 event line(s) were not records",
		"41 passed, 1 failed, 2 skipped (3 dropped by the test runner)",
		"FAIL example.com/a TestParse go/libs/a/parse_test.go:41",
		"line a",
		"… 5 more line(s)",
		"… 4 case(s) left out by the document's bounds",
		"… 5 further test task(s)",
		"65.5%",
		"threshold 70%  BELOW",
		"error go/libs/a/x.go:7:3 value never used (SA4006)",
		"… 6 more diagnostic(s)",
		"oci.putnami.dev/putnami/svc@1.0.0  docker  sha256:abc  tags canary",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("results output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TestOK") {
		t.Fatalf("results output listed a passed case; they are counted, not listed:\n%s", out)
	}
	if strings.Contains(out, "line u") {
		t.Fatalf("results output printed a failed case's output past its line bound:\n%s", out)
	}
}

// --output=json emits the whole document, every kept case included.
func TestCIResultsEmitsTheDocumentAsJSON(t *testing.T) {
	fake := ciResultsFake(ciStubResponse{status: 200, body: map[string]any{
		"runId": "run-9",
		"tests": []map[string]any{{
			"key": "/p:test~test", "project": "/p", "task": "test~test", "passed": 1,
			"cases": []map[string]any{{"name": "TestOK", "suite": "p", "status": "passed", "durationMs": 1}},
		}},
	}})
	lines, err := captureCI(t, fake, []string{"results", "run-9", "--output", "json"})
	if err != nil {
		t.Fatalf("results --output=json: %v", err)
	}
	var doc CIRunResultsResponse
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &doc); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if doc.RunID != "run-9" || len(doc.Tests) != 1 || len(doc.Tests[0].Cases) != 1 {
		t.Fatalf("json = %+v, want the whole document", doc)
	}
}

// A run with no results is a different answer from a run that does not exist.
func TestCIResultsExplainsAMissingDocument(t *testing.T) {
	fake := ciResultsFake(ciStubResponse{
		status: http.StatusNotFound, body: map[string]string{"error": "run results not found"},
	})
	_, err := captureCI(t, fake, []string{"results", "run-9"})
	if err == nil || !strings.Contains(err.Error(), "carries no results") {
		t.Fatalf("error = %v, want it to say the run carries no results", err)
	}
	if _, err := captureCI(t, fake, []string{"results"}); err == nil {
		t.Fatal("results with no run id succeeded, want a usage error")
	}
}
