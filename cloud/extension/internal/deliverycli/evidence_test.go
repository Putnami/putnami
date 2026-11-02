package deliverycli

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	evidenceFeature   = "cloud/evidence-upload"
	evidenceTestToken = "pkt_test_ingest_token"
)

// evidenceFakeServer stands in for Delivery's records routes. It records every
// request and answers each route with what the real one would.
type evidenceFakeServer struct {
	requests []reportCapturedRequest
	// raw holds the body of each request as it was sent.
	raw           [][]byte
	refuseRun     int
	refuseReports int
	// refuseOperational answers a run that carries operational as a control
	// plane that does not take it from a workspace token.
	refuseOperational bool
	// alreadyStored answers every kind as already stored when it is true.
	alreadyStored bool
}

func (s *evidenceFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	s.raw = append(s.raw, raw)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.requests = append(s.requests, reportCapturedRequest{Method: req.Method, Path: req.URL.Path, Auth: req.Header.Get("Authorization"), Body: body})
	switch req.URL.Path {
	case recordsIngestPath:
		if _, ok := body["operational"]; ok && s.refuseOperational {
			return reportJSONResponse(http.StatusBadRequest, map[string]any{"error": "operational metrics require a run credential", "message": "operational metrics require a run credential"}), nil
		}
		if s.refuseRun != 0 {
			return reportJSONResponse(s.refuseRun, map[string]any{"error": "changeRequest.host is invalid", "message": "changeRequest.host is invalid"}), nil
		}
		return reportJSONResponse(http.StatusOK, map[string]any{
			"runId": "run-1", "subjectId": "subject-1", "status": "completed",
			"conclusion": body["conclusion"], "provenance": body["provenance"], "verified": false,
		}), nil
	case recordsReportsIngestPath:
		if s.refuseReports != 0 {
			return reportJSONResponse(s.refuseReports, map[string]any{"error": "request body too large", "message": "request body too large"}), nil
		}
		var stored []string
		for _, kind := range []string{"tests", "coverage", "components", "changedLineCoverage", "dependencyChanges"} {
			if _, ok := body[kind]; ok {
				stored = append(stored, kind)
			}
		}
		answer := "stored"
		if s.alreadyStored {
			answer = "idempotent"
		}
		return reportJSONResponse(http.StatusOK, map[string]any{"runId": "run-1", "subjectId": "subject-1", answer: stored}), nil
	}
	return reportJSONResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
}

func (s *evidenceFakeServer) paths() []string {
	out := make([]string, 0, len(s.requests))
	for _, r := range s.requests {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func runEvidenceReport(t *testing.T, fake *evidenceFakeServer, env map[string]string, args ...string) (stdout string, err error) {
	t.Helper()
	stdout, _, err = runEvidenceReportLogged(t, fake, env, args...)
	return stdout, err
}

func runEvidenceReportLogged(t *testing.T, fake *evidenceFakeServer, env map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stubReportRetrySleep(t)
	if _, ok := env[evidenceInvocationDirEnv]; !ok && env != nil {
		// Git reads an empty directory, never the checkout the tests run in.
		env = maps.Clone(env)
		env[evidenceInvocationDirEnv] = t.TempDir()
	}
	var out, log []string
	ioctx := clicore.IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(line string) { log = append(log, line) },
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC) },
	}
	params := clicore.MergeParams(clicore.ParseFlags(args))
	err = EvidenceReport(params, env, ioctx)
	return strings.Join(out, "\n"), strings.Join(log, "\n"), err
}

func evidenceEnv() map[string]string {
	return map[string]string{evidenceTokenEnv: evidenceTestToken, "PUTNAMI_CLOUD_API_URL": reportTestBaseURL}
}

func evidenceFixture(name string) string {
	return filepath.Join("testdata", "evidence", name)
}

func TestEvidenceReportOpensAGitLabRunWithItsMergeRequest(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "opens-the-external-run", "a-gitlab-merge-request-opens-a-gitlab-run")
	fake := &evidenceFakeServer{}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("symfony-merge-request.json"))
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	run := fake.requests[0]
	if run.Path != recordsIngestPath || run.Auth != "Bearer "+evidenceTestToken {
		t.Fatalf("first request = %s with %q, want the run under the workspace token", run.Path, run.Auth)
	}
	want := map[string]any{
		"provider": "gitlab", "repo": "4521", "branch": "fix/invoice-rounding", "provenance": "gitlab",
		"sha": "4f6c2a9e1b7d3c5a8e0f2b4d6a8c0e1f3a5b7c9d", "status": "completed", "conclusion": "success",
		"submissionId": "observe-gitlab-4521-123456",
	}
	for key, value := range want {
		if run.Body[key] != value {
			t.Errorf("run %s = %v, want %v", key, run.Body[key], value)
		}
	}
	ref, _ := run.Body["changeRequest"].(map[string]any)
	if ref["provider"] != "gitlab" || ref["host"] != "gitlab.example.com" || ref["number"] != float64(7) {
		t.Fatalf("changeRequest = %v, want gitlab merge request 7 on gitlab.example.com", run.Body["changeRequest"])
	}
	if _, ok := run.Body["pullRequest"]; ok {
		t.Fatalf("run carries pullRequest %v beside its changeRequest", run.Body["pullRequest"])
	}
	if !strings.Contains(stdout, "run run-1 (gitlab, self-reported)") {
		t.Fatalf("stdout = %q, want the run and its provenance", stdout)
	}
}

func TestEvidenceReportOpensAGHARunForAPullRequest(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "opens-the-external-run", "a-github-pull-request-opens-a-gha-run")
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	run := fake.requests[0].Body
	if run["provenance"] != "gha" || run["provider"] != "github" || run["repo"] != "812345678" {
		t.Fatalf("run = %v, want a gha run of github project 812345678", run)
	}
	if run["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure: a test of packages/ui failed", run["conclusion"])
	}
	id, _ := run["submissionId"].(string)
	leg, ok := strings.CutPrefix(id, "observe-gha-812345678-9876543210-1-27719033811-")
	if !ok || len(leg) != 12 {
		t.Fatalf("submissionId = %v, want the run, attempt and job of the CI and the leg's digest", run["submissionId"])
	}
	ref, _ := run["changeRequest"].(map[string]any)
	if ref["provider"] != "github" || ref["host"] != "github.com" || ref["number"] != float64(42) {
		t.Fatalf("changeRequest = %v, want the github change request numbered 42", run["changeRequest"])
	}
}

func TestEvidenceReportPostsTestsAndCoverageThenTheEvidenceMembers(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "posts-the-reports-in-two-batches", "tests-and-coverage-then-the-evidence-members")
	fake := &evidenceFakeServer{}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"))
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	want := []string{"POST " + recordsIngestPath, "POST " + recordsReportsIngestPath, "POST " + recordsReportsIngestPath}
	if got := fake.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	reports, members := fake.requests[1].Body, fake.requests[2].Body
	submission := fake.requests[0].Body["submissionId"]
	if reports["submissionId"] != submission || members["submissionId"] != submission {
		t.Fatalf("batches carry %v and %v, want the run's submission id %v", reports["submissionId"], members["submissionId"], submission)
	}
	for _, key := range []string{"components", "changedLineCoverage", "dependencyChanges"} {
		if _, ok := reports[key]; ok {
			t.Fatalf("the tests batch carries %s", key)
		}
		if _, ok := members[key]; !ok {
			t.Fatalf("the evidence batch lacks %s: %v", key, members)
		}
	}
	for _, key := range []string{"tests", "coverage"} {
		if _, ok := members[key]; ok {
			t.Fatalf("the evidence batch carries %s", key)
		}
	}

	var fixture map[string]any
	data, err := os.ReadFile(evidenceFixture("turbo-pull-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"components", "changedLineCoverage", "dependencyChanges"} {
		got, _ := json.Marshal(members[key])
		wantJSON, _ := json.Marshal(fixture[key])
		if string(got) != string(wantJSON) {
			t.Fatalf("%s = %s, want the document's own %s", key, got, wantJSON)
		}
	}

	projects := reports["tests"].(map[string]any)["projects"].([]any)
	ui := projects[0].(map[string]any)
	if ui["project"] != "packages/ui" || ui["passed"] != float64(41) || ui["failed"] != float64(1) || ui["skipped"] != float64(2) || ui["total"] != float64(44) {
		t.Fatalf("tests of packages/ui = %v", ui)
	}
	failing := ui["failingCases"].([]any)[0].(map[string]any)
	if failing["name"] != "renders the count" || failing["file"] != "packages/ui/src/badge.test.tsx" {
		t.Fatalf("failing case = %v", failing)
	}
	coverage := reports["coverage"].(map[string]any)["projects"].([]any)[0].(map[string]any)
	fixtureCoverage := fixture["coverage"].([]any)[0].(map[string]any)
	if coverage["project"] != fixtureCoverage["component"] || coverage["coveredStatements"] != fixtureCoverage["coveredLines"] || coverage["totalStatements"] != fixtureCoverage["totalLines"] {
		t.Fatalf("coverage = %v, want the lines of %v", coverage, fixtureCoverage)
	}
	if !strings.Contains(stdout, "stored tests, coverage, components, changedLineCoverage, dependencyChanges") {
		t.Fatalf("stdout = %q, want every stored kind", stdout)
	}
}

func TestEvidenceReportSendsNoEmptyBatch(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "posts-the-reports-in-two-batches", "tests-and-coverage-then-the-evidence-members")
	doc := map[string]any{}
	data, err := os.ReadFile(evidenceFixture("symfony-merge-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "tests")
	delete(doc, "coverage")
	path := writeEvidenceDoc(t, doc)
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", path); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if got := fake.paths(); len(got) != 2 {
		t.Fatalf("requests = %v, want the run and the evidence batch alone", got)
	}
}

func writeEvidenceDoc(t *testing.T, doc any) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvidenceReportSendsNothingItCannotUpload(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "refuses-what-it-cannot-upload", "nothing-is-sent-without-a-token-or-a-valid-document")
	genericPath := evidenceFixture("generic-minimal.json")
	invalid := writeEvidenceDoc(t, map[string]any{"protocolVersion": 1, "subject": map[string]any{"provider": "gitlab"}})
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		code int
		want string
	}{
		"no token":        {map[string]string{"PUTNAMI_CLOUD_API_URL": reportTestBaseURL}, []string{"--file", evidenceFixture("symfony-merge-request.json")}, clicore.ExitAuth, evidenceTokenEnv},
		"two tokens":      {map[string]string{evidenceTokenEnv: "pkt_a pkt_b"}, []string{"--file", evidenceFixture("symfony-merge-request.json")}, clicore.ExitAuth, "exactly one bearer"},
		"no file":         {evidenceEnv(), nil, clicore.ExitUsage, "--file"},
		"a missing file":  {evidenceEnv(), []string{"--file", filepath.Join(t.TempDir(), "none.json")}, clicore.ExitUsage, "cannot read"},
		"an invalid file": {evidenceEnv(), []string{"--file", invalid}, clicore.ExitUsage, "not a valid change evidence document"},
		"a generic CI":    {evidenceEnv(), []string{"--file", genericPath}, clicore.ExitUsage, "no provenance"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &evidenceFakeServer{}
			_, err := runEvidenceReport(t, fake, tc.env, tc.args...)
			if err == nil || clicore.ExitCode(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (exit %d), want exit %d naming %q", err, clicore.ExitCode(err), tc.code, tc.want)
			}
			if len(fake.requests) != 0 {
				t.Fatalf("requests = %v, want none", fake.paths())
			}
		})
	}
}

func TestEvidenceReportNamesTheRouteAndTheReasonOfARefusal(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "refuses-what-it-cannot-upload", "a-refused-call-names-the-route-and-the-reason")
	fake := &evidenceFakeServer{refuseRun: http.StatusBadRequest}
	_, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("symfony-merge-request.json"))
	if err == nil || !strings.Contains(err.Error(), reportTestBaseURL+recordsIngestPath) || !strings.Contains(err.Error(), "changeRequest.host is invalid") {
		t.Fatalf("err = %v, want the route and the server's reason", err)
	}
	if strings.Contains(err.Error(), evidenceTestToken) {
		t.Fatalf("err = %v carries the token", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %v, want the refused run alone, without a retry or a batch", fake.paths())
	}

	fake = &evidenceFakeServer{refuseReports: http.StatusRequestEntityTooLarge}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"))
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), recordsReportsIngestPath) || !strings.Contains(err.Error(), "request body too large") {
		t.Fatalf("err = %v, want the refused batch route and reason", err)
	}
	if !strings.Contains(stdout, "run run-1") || strings.Contains(err.Error(), evidenceTestToken) {
		t.Fatalf("stdout = %q, err = %v: want the opened run and no token", stdout, err)
	}
}

func TestEvidenceReportOpensOneRunPerMatrixLeg(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "opens-the-external-run", "a-github-pull-request-opens-a-gha-run")
	doc := map[string]any{}
	data, err := os.ReadFile(evidenceFixture("turbo-pull-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	submission := func(doc map[string]any) string {
		t.Helper()
		fake := &evidenceFakeServer{}
		if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", writeEvidenceDoc(t, doc)); err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		id, _ := fake.requests[0].Body["submissionId"].(string)
		return id
	}
	first := submission(doc)
	doc["generatedAt"] = "2026-11-03T09:00:00Z"
	if again := submission(doc); again != first {
		t.Fatalf("the same leg uploaded again opens %s, want %s", again, first)
	}
	delete(doc, "tests")
	if other := submission(doc); other == first {
		t.Fatalf("another leg of the matrix shares the submission id %s", first)
	}
}

func TestEvidenceReportSaysWhatWasAlreadyStored(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "posts-the-reports-in-two-batches", "tests-and-coverage-then-the-evidence-members")
	fake := &evidenceFakeServer{alreadyStored: true}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"))
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if !strings.Contains(stdout, "stored no report; already stored tests, coverage, components") {
		t.Fatalf("stdout = %q, want the kinds that were already stored apart", stdout)
	}
}

// evidenceRequestsTranscript lists each request the fake received: its method
// and path on one line, then its body as it was sent.
func evidenceRequestsTranscript(fake *evidenceFakeServer) []byte {
	var out []byte
	for i, request := range fake.requests {
		out = append(out, request.Method+" "+request.Path+"\n"...)
		out = append(out, fake.raw[i]...)
		if len(fake.raw[i]) == 0 || fake.raw[i][len(fake.raw[i])-1] != '\n' {
			out = append(out, '\n')
		}
	}
	return out
}

func TestEvidenceReportSendsTheStatesOfEachDependencyChange(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-dependency-states", "a-dependency-change-carries-its-before-and-after-states")
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("sbom-pull-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if got := fake.paths(); len(got) != 2 || got[1] != "POST "+recordsReportsIngestPath {
		t.Fatalf("requests = %v, want the run and the evidence batch", got)
	}
	members := fake.requests[1].Body
	if _, ok := members["sbom"]; ok {
		t.Fatalf("the evidence batch carries the document's sbom member: %v", members)
	}

	var fixture map[string]any
	data, err := os.ReadFile(evidenceFixture("sbom-pull-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(members["dependencyChanges"])
	want, _ := json.Marshal(fixture["dependencyChanges"])
	if string(got) != string(want) {
		t.Fatalf("dependencyChanges = %s, want the document's own %s", got, want)
	}

	changes := members["dependencyChanges"].([]any)
	lodash := changes[0].(map[string]any)
	before, _ := lodash["before"].(map[string]any)
	after, _ := lodash["after"].(map[string]any)
	if before == nil || after == nil {
		t.Fatalf("lodash = %v, want its before and after states", lodash)
	}
	fixed, _ := before["vulnerabilities"].([]any)
	if len(fixed) != 2 || fixed[1].(map[string]any)["id"] != "GHSA-35jh-r3h4-6jhm" || fixed[1].(map[string]any)["severity"] != "high" {
		t.Fatalf("lodash before = %v, want its two known vulnerabilities with id and severity", before)
	}
	if licenses, _ := after["licenses"].([]any); len(licenses) != 1 || licenses[0] != "MIT" {
		t.Fatalf("lodash after = %v, want its MIT license", after)
	}
	if _, ok := after["vulnerabilities"]; ok {
		t.Fatalf("lodash after = %v, want no vulnerability member", after)
	}
	if removed := changes[1].(map[string]any); removed["after"] != nil || removed["before"] == nil {
		t.Fatalf("minimist = %v, want its before state alone", removed)
	}
	if added := changes[2].(map[string]any); added["before"] != nil || added["after"] == nil {
		t.Fatalf("zod = %v, want its after state alone", added)
	}
	if plain := changes[3].(map[string]any); plain["before"] != nil || plain["after"] != nil {
		t.Fatalf("left-pad = %v, want no state", plain)
	}
}

func TestEvidenceReportUploadsADocumentWithoutStatesAsItDidBefore(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-dependency-states", "a-document-without-states-uploads-the-same-body")
	for _, name := range []string{"turbo-pull-request", "symfony-merge-request"} {
		t.Run(name, func(t *testing.T) {
			fake := &evidenceFakeServer{}
			if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture(name+".json")); err != nil {
				t.Fatalf("evidence-report: %v", err)
			}
			want, err := os.ReadFile(evidenceFixture(name + ".requests.golden"))
			if err != nil {
				t.Fatal(err)
			}
			if got := evidenceRequestsTranscript(fake); string(got) != string(want) {
				t.Fatalf("requests =\n%s\nwant the bodies the upload sent before dependency states existed:\n%s", got, want)
			}
		})
	}
}
