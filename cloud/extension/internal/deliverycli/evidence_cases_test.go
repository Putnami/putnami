package deliverycli

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// evidenceTestsProjects returns the projects of the tests report the batch
// carries, keyed by project, or nil when it carries none.
func evidenceTestsProjects(body map[string]any) map[string]map[string]any {
	tests, ok := body["tests"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for _, project := range tests["projects"].([]any) {
		project := project.(map[string]any)
		out[project["project"].(string)] = project
	}
	return out
}

func TestEvidenceReportSendsTheCasesOfEachTestsEntry(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-test-cases", "a-tests-entry-carries-its-cases")
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if got := fake.paths(); len(got) < 2 || got[1] != "POST "+recordsReportsIngestPath {
		t.Fatalf("requests = %v, want the run and its report batches", got)
	}
	projects := evidenceTestsProjects(fake.requests[1].Body)
	if len(projects) != 2 {
		t.Fatalf("tests = %v, want both components", fake.requests[1].Body["tests"])
	}

	var fixture struct {
		Tests []struct {
			Component    string          `json:"component"`
			Cases        json.RawMessage `json:"cases"`
			OmittedCases float64         `json:"omittedCases"`
		} `json:"tests"`
	}
	data, err := os.ReadFile(evidenceFixture("test-cases-pull-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, entry := range fixture.Tests {
		project := projects[entry.Component]
		got, _ := json.Marshal(project["cases"])
		var want any
		_ = json.Unmarshal(entry.Cases, &want)
		wantJSON, _ := json.Marshal(want)
		if string(got) != string(wantJSON) {
			t.Errorf("%s cases = %s, want the document's own %s", entry.Component, got, wantJSON)
		}
		omitted, present := project["omittedCases"]
		switch {
		case entry.OmittedCases == 0 && present:
			t.Errorf("%s carries omittedCases %v, want none", entry.Component, omitted)
		case entry.OmittedCases != 0 && omitted != entry.OmittedCases:
			t.Errorf("%s omittedCases = %v, want %v", entry.Component, omitted, entry.OmittedCases)
		}
	}
	if web := projects["apps/web"]; web["total"] != float64(118) || web["passed"] != float64(118) {
		t.Errorf("apps/web = %v, want its counts beside its cases", web)
	}
}

func TestEvidenceReportSendsNoCasesForAnEntryWithout(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-test-cases", "a-tests-entry-without-cases-uploads-the-same-body")
	// Neither fixture lists a case, and TestEvidenceReportRequestsMatchGolden
	// holds their requests to the committed golden bodies.
	for _, fixture := range []string{"turbo-pull-request.json", "symfony-merge-request.json"} {
		fake := &evidenceFakeServer{}
		if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture(fixture)); err != nil {
			t.Fatalf("%s: evidence-report: %v", fixture, err)
		}
		sawTests := false
		for index, request := range fake.requests {
			for _, member := range []string{`"cases"`, `"omittedCases"`} {
				if strings.Contains(string(fake.raw[index]), member) {
					t.Errorf("%s: %s carries %s: %s", fixture, request.Path, member, fake.raw[index])
				}
			}
			sawTests = sawTests || evidenceTestsProjects(request.Body) != nil
		}
		if !sawTests {
			t.Fatalf("%s: no batch carries the tests report", fixture)
		}
	}
}

func TestEvidenceReportPostsARefusedBatchAgainWithoutTheCases(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-test-cases", "a-refused-batch-is-posted-again-without-the-cases")
	t.Run("too large with its cases", func(t *testing.T) {
		fake := &evidenceFakeServer{refuseCases: true}
		stdout, stderr, err := runEvidenceReportLogged(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json"), "--output=json")
		if err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		got := fake.paths()
		if len(got) < 3 || got[1] != "POST "+recordsReportsIngestPath || got[2] != "POST "+recordsReportsIngestPath {
			t.Fatalf("requests = %v, want the run, the refused batch and the batch again", got)
		}
		again := fake.requests[2].Body
		if again["submissionId"] != fake.requests[1].Body["submissionId"] {
			t.Fatalf("the batch goes again under %v, want %v", again["submissionId"], fake.requests[1].Body["submissionId"])
		}
		if strings.Contains(string(fake.raw[2]), `"cases"`) || strings.Contains(string(fake.raw[2]), `"omittedCases"`) {
			t.Fatalf("the batch goes again with its cases: %s", fake.raw[2])
		}
		projects := evidenceTestsProjects(again)
		if ui := projects["packages/ui"]; ui["failed"] != float64(1) || ui["failingCases"] == nil {
			t.Fatalf("packages/ui = %v, want its counts and failing cases", ui)
		}
		if _, ok := again["coverage"]; !ok {
			t.Fatalf("the batch goes again without its coverage: %v", again)
		}
		if !strings.Contains(stderr, "sends no test cases") || !strings.Contains(stderr, "exceeds the declared bound") {
			t.Fatalf("stderr = %q, want a warning that names the refusal", stderr)
		}
		if !strings.Contains(stdout, `"test_cases_left_out"`) || !strings.Contains(stdout, "exceeds the declared bound") {
			t.Fatalf("stdout = %q, want the refusal under test_cases_left_out", stdout)
		}
	})

	t.Run("refused for the cases", func(t *testing.T) {
		fake := &evidenceFakeServer{refuseCasesStatus: http.StatusBadRequest}
		_, stderr, err := runEvidenceReportLogged(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json"))
		if err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		if got := fake.paths(); len(got) != 4 {
			t.Fatalf("requests = %v, want the run, the refused batch, the batch again and the evidence batch", got)
		}
		if strings.Contains(string(fake.raw[2]), `"cases"`) || evidenceTestsProjects(fake.requests[2].Body) == nil {
			t.Fatalf("the batch goes again as %s, want its tests without cases", fake.raw[2])
		}
		if !strings.Contains(stderr, "sends no test cases") {
			t.Fatalf("stderr = %q, want a warning that names the refusal", stderr)
		}
	})

	t.Run("refused again", func(t *testing.T) {
		fake := &evidenceFakeServer{refuseReports: http.StatusRequestEntityTooLarge}
		stdout, stderr, err := runEvidenceReportLogged(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json"), "--output=json")
		if err == nil || clicore.ExitCode(err) != clicore.ExitAPI {
			t.Fatalf("err = %v, want the refusal of the batch without cases", err)
		}
		if got := fake.paths(); len(got) != 4 {
			t.Fatalf("requests = %v, want the run, the batch, the batch again and the evidence batch", got)
		}
		if strings.Contains(stderr, "sends no test cases") {
			t.Fatalf("stderr = %q: cases reported left out of a batch that was not stored", stderr)
		}
		data, _ := protocolcli.ResultData(err).(map[string]any)
		if data == nil {
			t.Fatalf("err = %v carries no result data; stdout = %q", err, stdout)
		}
		if _, ok := data["test_cases_left_out"]; ok {
			t.Fatalf("result data = %v names cases left out of a batch that was not stored", data)
		}
	})
}

func TestEvidenceReportLeavesOutTheLargestCasesOverTheBound(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-test-cases", "cases-over-the-bound-are-left-out-largest-first")
	// packages/ui lists five cases and apps/web two: a bound between the two
	// lists' sizes leaves out packages/ui's alone.
	defer func(budget int) { evidenceTestCasesBudget = budget }(evidenceTestCasesBudget)
	evidenceTestCasesBudget = 200
	fake := &evidenceFakeServer{}
	stdout, stderr, err := runEvidenceReportLogged(t, fake, evidenceEnv(), "--file", evidenceFixture("test-cases-pull-request.json"), "--output=json")
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	projects := evidenceTestsProjects(fake.requests[1].Body)
	ui, web := projects["packages/ui"], projects["apps/web"]
	if _, ok := ui["cases"]; ok || ui["omittedCases"] != float64(5) {
		t.Fatalf("packages/ui = %v, want no cases and its five counted omitted", ui)
	}
	if cases, _ := web["cases"].([]any); len(cases) != 2 || web["omittedCases"] != float64(116) {
		t.Fatalf("apps/web = %v, want its two cases and 116 omitted", web)
	}
	if !strings.Contains(stderr, "sends no test cases for packages/ui") {
		t.Fatalf("stderr = %q, want a warning that names packages/ui", stderr)
	}
	if !strings.Contains(stdout, `"test_cases_shed"`) || !strings.Contains(stdout, "packages/ui") {
		t.Fatalf("stdout = %q, want packages/ui under test_cases_shed", stdout)
	}
}
