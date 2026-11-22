package deliverycli

import (
	"strings"
	"testing"
)

// Delivery's tests report is closed (additionalProperties: false) and defines
// no case list yet, so the upload keeps the evidence's test cases out of
// every request until Delivery accepts them.
func TestEvidenceReportSendsNoTestCasesDeliveryDoesNotDefine(t *testing.T) {
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if len(fake.requests) < 2 {
		t.Fatalf("requests = %v, want the run and its report batches", fake.paths())
	}
	sawTests := false
	for index, request := range fake.requests {
		body := fake.raw[index]
		for _, member := range []string{`"cases"`, `"omittedCases"`} {
			if strings.Contains(string(body), member) {
				t.Errorf("%s carries %s: %s", request.Path, member, body)
			}
		}
		if tests, ok := request.Body["tests"].(map[string]any); ok {
			sawTests = true
			if projects, _ := tests["projects"].([]any); len(projects) != 2 {
				t.Errorf("tests = %v, want both components' counts", tests)
			}
		}
	}
	if !sawTests {
		t.Fatal("no batch carries the tests report")
	}
}
