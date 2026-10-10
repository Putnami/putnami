package deliverycli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/reportstream"
)

const reportTestBaseURL = "https://control.test"

// fakeGit builds a reportGitRun seam from canned command outputs keyed by the
// joined git argv (e.g. "rev-parse HEAD"). Unlisted commands fail the test, so
// a refusal path can prove it never even reached later git calls.
type fakeGit struct {
	t    *testing.T
	outs map[string]string
	errs map[string]error
}

func (f *fakeGit) run(_ string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	out, ok := f.outs[key]
	if !ok {
		f.t.Fatalf("unexpected git call: git %s", key)
	}
	return out, nil
}

// cleanGitOutputs is a pushed, clean HEAD on a branch with an origin remote.
func cleanGitOutputs() map[string]string {
	return map[string]string{
		"rev-parse HEAD":     "0123456789abcdef0123456789abcdef01234567",
		"status --porcelain": "",
		"branch -r --contains 0123456789abcdef0123456789abcdef01234567": "  origin/main",
		"rev-parse --abbrev-ref HEAD":                                   "main",
		"remote get-url origin":                                         "git@github.com:acme/app.git",
	}
}

func stubReportGit(t *testing.T, git *fakeGit) {
	t.Helper()
	prev := reportGitRun
	reportGitRun = git.run
	t.Cleanup(func() { reportGitRun = prev })
}

func stubReportWrapped(t *testing.T, exit int, err error) *[][]string {
	t.Helper()
	return stubReportWrappedJSONL(t, exit, err, "")
}

// stubReportWrappedJSONL stubs the wrapped-command seam and, when jsonl is
// non-empty, writes it to the tee — simulating a wrapped `putnami …
// --output=jsonl` run whose stdout is tee'd into the report parser.
func stubReportWrappedJSONL(t *testing.T, exit int, err error, jsonl string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := reportRunWrapped
	reportRunWrapped = func(argv []string, _ string, tee io.Writer) (int, error) {
		calls = append(calls, argv)
		if tee != nil && jsonl != "" {
			_, _ = io.WriteString(tee, jsonl)
		}
		return exit, err
	}
	t.Cleanup(func() { reportRunWrapped = prev })
	return &calls
}

func stubReportRetrySleep(t *testing.T) {
	t.Helper()
	prev := reportRetrySleep
	reportRetrySleep = func(int) {}
	t.Cleanup(func() { reportRetrySleep = prev })
}

// reportFakeServer stands in for the control plane's ingest endpoints plus the
// auth endpoints WorkspaceAuth needs. failFirst makes the first ingest POST a
// 503 so the retry path is observable; failReports makes every report-batch
// POST a 500 so the fail-soft contract is observable.
type reportFakeServer struct {
	requests       []reportCapturedRequest
	reportRequests []reportCapturedRequest
	failFirst      bool
	failReports    bool
	reportPartial  bool
}

type reportCapturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func (s *reportFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	switch {
	case req.URL.Path == "/.well-known/openid-configuration":
		return reportJSONResponse(http.StatusOK, map[string]any{
			"issuer": reportTestBaseURL, "token_endpoint": reportTestBaseURL + "/token",
			"userinfo_endpoint": reportTestBaseURL + "/userinfo",
		}), nil
	case req.URL.Path == "/token":
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		extra := map[string]any{}
		if workspaceID := clicore.StringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return reportJSONResponse(http.StatusOK, reportTokenResponse(extra)), nil
	case req.URL.Path == recordsReportsIngestPath:
		var body map[string]any
		if len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &body)
		}
		s.reportRequests = append(s.reportRequests, reportCapturedRequest{
			Method: req.Method,
			Path:   req.URL.Path,
			Auth:   req.Header.Get("Authorization"),
			Body:   body,
		})
		if s.failReports {
			return reportJSONResponse(http.StatusInternalServerError, map[string]any{"error": "reports storage down"}), nil
		}
		var stored []string
		for _, kind := range []string{"tests", "coverage", "builds"} {
			if _, ok := body[kind]; ok {
				stored = append(stored, kind)
			}
		}
		return reportJSONResponse(http.StatusOK, map[string]any{
			"runId":     "run-1",
			"subjectId": "subject-1",
			"stored":    stored,
			"partial":   s.reportPartial,
		}), nil
	case req.URL.Path == recordsIngestPath:
		var body map[string]any
		if len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &body)
		}
		s.requests = append(s.requests, reportCapturedRequest{
			Method: req.Method,
			Path:   req.URL.Path,
			Auth:   req.Header.Get("Authorization"),
			Body:   body,
		})
		if s.failFirst && len(s.requests) == 1 {
			return reportJSONResponse(http.StatusServiceUnavailable, map[string]any{"error": "try again"}), nil
		}
		return reportJSONResponse(http.StatusOK, map[string]any{
			"runId":      "run-1",
			"subjectId":  "subject-1",
			"status":     clicore.StringValue(body["status"]),
			"conclusion": clicore.StringValue(body["conclusion"]),
			"provenance": clicore.StringValue(body["provenance"]),
			"verified":   false,
			"idempotent": len(s.requests) > 1,
		}), nil
	default:
		return reportJSONResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
}

func newReportTestIO(t *testing.T, fake *reportFakeServer) (clicore.IO, string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeReportTestAuth(t, home)
	writeReportLinkFile(t, workspaceRoot)
	return clicore.IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": reportTestBaseURL,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC) },
	}, workspaceRoot
}

func runReport(t *testing.T, ioctx clicore.IO, workspaceRoot string, args []string) error {
	t.Helper()
	stubReportRetrySleep(t)
	params := clicore.MergeParams(clicore.ParseFlags(args))
	return Report(params, args, workspaceRoot, ioctx.Env, ioctx)
}

func TestReportSubmitsConclusionForPushedCommit(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

	if err := runReport(t, ioctx, root, []string{"--conclusion", "success"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want exactly 1 ingest POST", fake.requests)
	}
	got := fake.requests[0]
	if got.Method != http.MethodPost || got.Path != recordsIngestPath {
		t.Fatalf("request = %s %s, want POST %s", got.Method, got.Path, recordsIngestPath)
	}
	if !strings.HasPrefix(got.Auth, "Bearer ") {
		t.Fatalf("authorization = %q, want a bearer", got.Auth)
	}
	if ws := clicore.StringValue(got.Body["workspace"]); ws != "ws-acme" {
		t.Fatalf("workspace assertion = %q, want ws-acme (the linked workspace)", ws)
	}
	if repo := clicore.StringValue(got.Body["repo"]); repo != "acme/app" {
		t.Fatalf("repo = %q, want acme/app", repo)
	}
	if provider := clicore.StringValue(got.Body["provider"]); provider != "github" {
		t.Fatalf("provider = %q, want github", provider)
	}
	if sha := clicore.StringValue(got.Body["sha"]); sha != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("sha = %q, want the resolved HEAD", sha)
	}
	if branch := clicore.StringValue(got.Body["branch"]); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if status := clicore.StringValue(got.Body["status"]); status != "completed" {
		t.Fatalf("status = %q, want completed (the default)", status)
	}
	if conclusion := clicore.StringValue(got.Body["conclusion"]); conclusion != "success" {
		t.Fatalf("conclusion = %q, want success", conclusion)
	}
	if sub := clicore.StringValue(got.Body["submissionId"]); !strings.HasPrefix(sub, "sub_") || len(sub) != len("sub_")+32 {
		t.Fatalf("submissionId = %q, want sub_ + 32 hex chars", sub)
	}
}

func TestReportRefusesDirtyTree(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	outs := cleanGitOutputs()
	outs["status --porcelain"] = " M internal/wire/wire.go"
	stubReportGit(t, &fakeGit{t: t, outs: outs})

	err := runReport(t, ioctx, root, []string{"--conclusion", "success"})
	if err == nil {
		t.Fatal("expected a refusal for the dirty working tree")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d (%v)", clicore.ExitCode(err), clicore.ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error should name the dirty tree and be actionable: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want NONE — refusal must happen client-side before any request", fake.requests)
	}
}

func TestReportRefusesUnpushedHead(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	outs := cleanGitOutputs()
	outs["branch -r --contains 0123456789abcdef0123456789abcdef01234567"] = ""
	stubReportGit(t, &fakeGit{t: t, outs: outs})

	err := runReport(t, ioctx, root, []string{"--conclusion", "success"})
	if err == nil {
		t.Fatal("expected a refusal for the unpushed HEAD")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d (%v)", clicore.ExitCode(err), clicore.ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "not on any remote branch") {
		t.Fatalf("error should name the unpushed HEAD and be actionable: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want NONE — refusal must happen client-side before any request", fake.requests)
	}
}

func TestReportProvenanceDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "local by default", env: nil, want: "local"},
		{name: "gha under GitHub Actions", env: map[string]string{"GITHUB_ACTIONS": "true"}, want: "gha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &reportFakeServer{}
			ioctx, root := newReportTestIO(t, fake)
			for k, v := range tc.env {
				ioctx.Env[k] = v
			}
			stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

			if err := runReport(t, ioctx, root, []string{"--conclusion", "success"}); err != nil {
				t.Fatalf("report: %v", err)
			}
			if got := clicore.StringValue(fake.requests[0].Body["provenance"]); got != tc.want {
				t.Fatalf("provenance = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReportWrappedCommandSuccess(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	calls := stubReportWrapped(t, 0, nil)

	if err := runReport(t, ioctx, root, []string{"--", "go", "test", "./..."}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "go test ./..." {
		t.Fatalf("wrapped calls = %+v, want [go test ./...]", *calls)
	}
	if got := clicore.StringValue(fake.requests[0].Body["conclusion"]); got != "success" {
		t.Fatalf("conclusion = %q, want success for exit 0", got)
	}
}

func TestReportWrappedCommandFailurePropagatesExitCode(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrapped(t, 3, nil)

	err := runReport(t, ioctx, root, []string{"--", "go", "test", "./..."})
	if err == nil {
		t.Fatal("expected the wrapped command's failure to propagate")
	}
	if clicore.ExitCode(err) != 3 {
		t.Fatalf("exit code = %d, want the wrapped command's 3 (%v)", clicore.ExitCode(err), err)
	}
	// The failure is still REPORTED (that's the point of the wrapper form).
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want the failure submitted", fake.requests)
	}
	if got := clicore.StringValue(fake.requests[0].Body["conclusion"]); got != "failure" {
		t.Fatalf("conclusion = %q, want failure for exit 3", got)
	}
}

func TestReportWrappedCommandFlagsDoNotLeakIntoParams(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrapped(t, 0, nil)

	// The wrapped command's own --conclusion must not collide with report's
	// outcome resolution (the dispatcher parses the whole argv into params).
	args := []string{"--", "sometool", "--conclusion", "bogus"}
	if err := runReport(t, ioctx, root, args); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got := clicore.StringValue(fake.requests[0].Body["conclusion"]); got != "success" {
		t.Fatalf("conclusion = %q, want success from the wrapped exit code, not the wrapped flag", got)
	}
}

func TestReportConclusionFromExitCodeFlag(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{code: "0", want: "success"},
		{code: "2", want: "failure"},
	} {
		t.Run("exit-code "+tc.code, func(t *testing.T) {
			fake := &reportFakeServer{}
			ioctx, root := newReportTestIO(t, fake)
			stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

			if err := runReport(t, ioctx, root, []string{"--exit-code", tc.code}); err != nil {
				t.Fatalf("report: %v", err)
			}
			if got := clicore.StringValue(fake.requests[0].Body["conclusion"]); got != tc.want {
				t.Fatalf("conclusion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReportRequiresConclusionSource(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

	err := runReport(t, ioctx, root, nil)
	if err == nil {
		t.Fatal("expected a usage error without a conclusion source")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d (%v)", clicore.ExitCode(err), clicore.ExitUsage, err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none", fake.requests)
	}
}

func TestReportRetryReusesSubmissionID(t *testing.T) {
	fake := &reportFakeServer{failFirst: true}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

	if err := runReport(t, ioctx, root, []string{"--conclusion", "success"}); err != nil {
		t.Fatalf("report should succeed on retry: %v", err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (503 then retry)", len(fake.requests))
	}
	first := clicore.StringValue(fake.requests[0].Body["submissionId"])
	second := clicore.StringValue(fake.requests[1].Body["submissionId"])
	if first == "" || first != second {
		t.Fatalf("submissionId across retries = %q vs %q, want the SAME id (idempotent retry)", first, second)
	}
}

// TestReportSubmitRequestWireKeys pins the CLI's wire body to the records
// ingest contract (the delivery API's SubmitRequest). The CLI module
// deliberately does not import the server package (GOWORK=off publish builds),
// so this golden guards the local twin against drift.
func TestReportSubmitRequestWireKeys(t *testing.T) {
	data, err := json.Marshal(reportSubmitRequest{
		Workspace:    "ws",
		Provider:     "github",
		Repo:         "acme/app",
		SHA:          "abc",
		Branch:       "main",
		Provenance:   "local",
		SubmissionID: "sub_1",
		Status:       "completed",
		Conclusion:   "success",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"workspace", "provider", "repo", "sha", "branch", "provenance", "submissionId", "status", "conclusion"}
	if len(doc) != len(want) {
		t.Fatalf("wire body has %d keys %v, want exactly %v", len(doc), doc, want)
	}
	for _, key := range want {
		if _, ok := doc[key]; !ok {
			t.Errorf("wire body missing key %q — keep in lockstep with the delivery API's SubmitRequest", key)
		}
	}
}

// testReportJSONL is a miniature real-shaped `putnami test --output=jsonl`
// stream (lines mirror the golden capture in internal/reportstream/testdata).
const testReportJSONL = `{"event":"job:start","package":"acme/libs/app","job":"test~test","time":"2026-07-03T13:21:14.537476421Z"}
{"event":"job:event","package":"acme/libs/app","job":"test~test","type":"metric","data":{"name":"tests-total","time":"2026-07-03T13:21:51.569Z","type":"metric","unit":"count","value":5}}
{"event":"job:event","package":"acme/libs/app","job":"test~test","type":"result","data":{"data":{"coverageSummary":{"covered":40,"coveredStatements":40,"fileCount":1,"files":[{"coveredStatements":40,"file":"go.example.com/acme/libs/app/app.go","percentage":80,"totalStatements":50}],"granularity":"statements","percentage":80,"total":50,"totalStatements":50,"uncoveredFiles":0},"testSummary":{"failed":0,"passed":5,"skipped":0,"total":5}},"level":"info","message":"Job OK","status":"OK","time":"2026-07-03T13:21:52.269Z","type":"result"},"time":"2026-07-03T13:21:52.269Z"}
{"event":"job:end","package":"acme/libs/app","job":"test~test","status":"success","duration":1200,"time":"2026-07-03T13:21:52.533111973Z","cache":false}
{"event":"session:end","success":true,"succeeded":1,"failed":0,"canceled":0,"skipped":0,"cached":0,"total":1,"duration":1300,"failures":[]}
`

// The wrapper form tees a JSONL stream and POSTs the parsed report batch after
// the run submission, on the same auth and the same submission id.
func TestReportWrappedJSONLSubmitsReportBatch(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, testReportJSONL)

	if err := runReport(t, ioctx, root, []string{"--", "putnami", "test", "--output=jsonl"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.requests) != 1 || len(fake.reportRequests) != 1 {
		t.Fatalf("run POSTs = %d, report POSTs = %d, want 1 and 1", len(fake.requests), len(fake.reportRequests))
	}
	batch := fake.reportRequests[0]
	if batch.Method != http.MethodPost || batch.Path != recordsReportsIngestPath {
		t.Fatalf("batch request = %s %s, want POST %s", batch.Method, batch.Path, recordsReportsIngestPath)
	}
	if !strings.HasPrefix(batch.Auth, "Bearer ") {
		t.Fatalf("batch authorization = %q, want the same bearer machinery as the run submission", batch.Auth)
	}
	runSub := clicore.StringValue(fake.requests[0].Body["submissionId"])
	if got := clicore.StringValue(batch.Body["submissionId"]); got == "" || got != runSub {
		t.Fatalf("batch submissionId = %q, want the run's %q (idempotency rides submission id + kind)", got, runSub)
	}
	if ws := clicore.StringValue(batch.Body["workspace"]); ws != "ws-acme" {
		t.Fatalf("workspace assertion = %q, want ws-acme", ws)
	}
	stream, _ := batch.Body["stream"].(map[string]any)
	// The provenance is the PARSER's contract revision, not the stream's: "2" is
	// the revision that folds putnami's v2 session records as well as the v1
	// events this fixture is written in.
	if clicore.StringValue(stream["version"]) != reportstream.ContractVersion {
		t.Fatalf("stream provenance = %v, want version %s", batch.Body["stream"], reportstream.ContractVersion)
	}
	tests, _ := batch.Body["tests"].(map[string]any)
	projects, _ := tests["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("tests payload = %v, want one project", batch.Body["tests"])
	}
	project, _ := projects[0].(map[string]any)
	if project["project"] != "acme/libs/app" || project["total"] != float64(5) || project["passed"] != float64(5) {
		t.Fatalf("tests project = %v", project)
	}
	coverage, _ := batch.Body["coverage"].(map[string]any)
	if covProjects, _ := coverage["projects"].([]any); len(covProjects) != 1 {
		t.Fatalf("coverage payload = %v", batch.Body["coverage"])
	}
	builds, _ := batch.Body["builds"].(map[string]any)
	tasks, _ := builds["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("builds payload = %v, want one task", batch.Body["builds"])
	}
	task, _ := tasks[0].(map[string]any)
	if task["task"] != "test~test" || task["status"] != "success" || task["durationMs"] != float64(1200) || task["cacheHit"] != false {
		t.Fatalf("builds task = %v", task)
	}
}

// testReportJSONLV2 is the same miniature stream written in putnami's v2
// session-record contract (protocols/cli SessionStreamRecord), the default
// since putnami 0.1.0-405dcd542. Lines mirror the real v2 capture in
// internal/reportstream/testdata/v2-lint-test-build.jsonl.
const testReportJSONLV2 = `{"protocolVersion":2,"record":"task:start","time":"2026-07-28T18:05:32.973653+02:00","identity":{"key":"/libs/app:test~test","scope":"project","project":{"id":"/libs/app","name":"acme/libs/app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}}}
{"protocolVersion":2,"record":"task:event","time":"2026-07-28T18:05:33.569Z","identity":{"key":"/libs/app:test~test","scope":"project","project":{"id":"/libs/app","name":"acme/libs/app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}},"event":{"name":"tests-total","time":"2026-07-28T18:05:33.569Z","type":"metric","unit":"count","value":5}}
{"protocolVersion":2,"record":"task:event","time":"2026-07-28T18:05:34.269Z","identity":{"key":"/libs/app:test~test","scope":"project","project":{"id":"/libs/app","name":"acme/libs/app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}},"event":{"data":{"coverageSummary":{"covered":40,"coveredStatements":40,"fileCount":1,"files":[{"coveredStatements":40,"file":"go.example.com/acme/libs/app/app.go","percentage":80,"totalStatements":50}],"granularity":"statements","percentage":80,"total":50,"totalStatements":50,"uncoveredFiles":0},"testSummary":{"failed":0,"passed":5,"skipped":0,"total":5}},"level":"info","message":"Job OK","status":"OK","time":"2026-07-28T18:05:34.269Z","type":"result"}}
{"protocolVersion":2,"record":"task:end","time":"2026-07-28T18:05:34.533111973+02:00","identity":{"key":"/libs/app:test~test","scope":"project","project":{"id":"/libs/app","name":"acme/libs/app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}},"task":{"identity":{"key":"/libs/app:test~test","scope":"project","project":{"id":"/libs/app","name":"acme/libs/app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}},"status":"success","reuse":"none","exitCode":0,"durationMs":1200,"taskWallMs":1250}}
{"protocolVersion":2,"record":"session:end","time":"2026-07-28T18:05:34.6+02:00","run":{"outcome":"success","exitCode":0,"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":1300}}
`

// The v2-default CLI once regressed here: every record was unknown
// to the parser, so the batch was empty and the aggregate check had nothing to
// stand on. The wrapper form must POST the SAME batch it POSTs for the v1 twin
// above — project key, counts, coverage and build task included.
func TestReportWrappedV2JSONLSubmitsSameBatchAsV1(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, testReportJSONLV2)

	if err := runReport(t, ioctx, root, []string{"--", "putnami", "test", "--output=jsonl"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.reportRequests) != 1 {
		t.Fatalf("report POSTs = %d, want 1 (an empty batch is never POSTed — the v2-default regression)", len(fake.reportRequests))
	}
	batch := fake.reportRequests[0]
	stream, _ := batch.Body["stream"].(map[string]any)
	if stream["lines"] != float64(5) || stream["unknownEvents"] != nil || stream["malformedLines"] != nil {
		t.Fatalf("stream provenance = %v, want 5 clean lines and no unknown records", batch.Body["stream"])
	}
	tests, _ := batch.Body["tests"].(map[string]any)
	projects, _ := tests["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("tests payload = %v, want one project", batch.Body["tests"])
	}
	project, _ := projects[0].(map[string]any)
	// The project key is the identity's project NAME, which is the exact string
	// v1's "package" carried — not the id "/libs/app" and not the plan key.
	if project["project"] != "acme/libs/app" || project["total"] != float64(5) || project["passed"] != float64(5) {
		t.Fatalf("tests project = %v", project)
	}
	coverage, _ := batch.Body["coverage"].(map[string]any)
	if covProjects, _ := coverage["projects"].([]any); len(covProjects) != 1 {
		t.Fatalf("coverage payload = %v", batch.Body["coverage"])
	}
	builds, _ := batch.Body["builds"].(map[string]any)
	tasks, _ := builds["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("builds payload = %v, want one task", batch.Body["builds"])
	}
	task, _ := tasks[0].(map[string]any)
	if task["project"] != "acme/libs/app" || task["task"] != "test~test" || task["status"] != "success" ||
		task["durationMs"] != float64(1200) || task["cacheHit"] != false {
		t.Fatalf("builds task = %v", task)
	}
}

// Fail-soft: a report batch the server refuses does not fail the command — the
// run's conclusion was already recorded and must stand.
func TestReportBatchIngestFailureIsFailSoft(t *testing.T) {
	fake := &reportFakeServer{failReports: true}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, testReportJSONL)

	if err := runReport(t, ioctx, root, []string{"--", "putnami", "test", "--output=jsonl"}); err != nil {
		t.Fatalf("report must succeed despite a failed report batch (fail-soft): %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("run POSTs = %d, want the run submission untouched", len(fake.requests))
	}
	if len(fake.reportRequests) == 0 {
		t.Fatal("the batch POST was never attempted")
	}
}

// The conclusion-only forms have no stream: no batch is POSTed and the
// existing path is unchanged.
func TestReportConclusionOnlySendsNoBatch(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})

	if err := runReport(t, ioctx, root, []string{"--conclusion", "success"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.reportRequests) != 0 {
		t.Fatalf("report POSTs = %+v, want none for the conclusion-only form", fake.reportRequests)
	}
}

// A wrapped command that emits no parseable JSONL (plain test output) yields
// no batch: junk is never submitted as a report.
func TestReportWrappedNonJSONLSendsNoBatch(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, "ok  \tgo.example.com/acme\t1.2s\nPASS\n")

	if err := runReport(t, ioctx, root, []string{"--", "go", "test", "./..."}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("run POSTs = %d, want 1", len(fake.requests))
	}
	if len(fake.reportRequests) != 0 {
		t.Fatalf("report POSTs = %+v, want none without report data", fake.reportRequests)
	}
}

// TestReportBatchWireKeys pins the CLI's batch wire body to the records
// reports ingest contract (the delivery API's ReportBatchRequest and
// the reports feature payload DTOs). The CLI module deliberately does not
// import the server packages (GOWORK=off publish builds), so this golden
// guards the local twins against drift. Its server-side mirror is
// TestReportBatchRequestWireKeys in the delivery API.
func TestReportBatchWireKeys(t *testing.T) {
	body := reportBatchRequest{
		Workspace:    "ws",
		SubmissionID: "sub_1",
		Stream:       reportBatchStream{Version: "1", Lines: 9, MalformedLines: 1, UnknownEvents: 2, OversizedLines: 3, ElidedLines: 4},
		Tests: &reportstream.TestsReport{Projects: []reportstream.ProjectTests{{
			Project: "p", Passed: 1, Failed: 1, Skipped: 1, Total: 3,
			FailingCases: []reportstream.FailingCase{{Name: "TestX", Message: "boom", File: "p/x_test.go", Line: 7}},
		}}},
		Coverage: &reportstream.CoverageReport{Projects: []reportstream.ProjectCoverage{{
			Project: "p", CoveredStatements: 1, TotalStatements: 2, Percentage: 50,
			Files: []reportstream.FileCoverage{{File: "p/x.go", CoveredStatements: 1, TotalStatements: 2, Percentage: 50}},
		}}},
		Builds: &reportstream.BuildsReport{Tasks: []reportstream.TaskBuild{{
			Project: "p", Task: "test~test", Status: "success", DurationMS: 5, CacheHit: true,
		}}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertExactKeys(t, "batch", doc, []string{"workspace", "submissionId", "stream", "tests", "coverage", "builds"})
	assertExactKeys(t, "stream", doc["stream"], []string{"version", "lines", "malformedLines", "unknownEvents", "oversizedLines", "elidedLines"})
	testsProject := firstProject(t, doc, "tests")
	assertExactKeys(t, "tests project", testsProject, []string{"project", "passed", "failed", "skipped", "total", "failingCases"})
	cases, _ := testsProject["failingCases"].([]any)
	assertExactKeys(t, "failing case", cases[0], []string{"name", "message", "file", "line"})
	covProject := firstProject(t, doc, "coverage")
	assertExactKeys(t, "coverage project", covProject, []string{"project", "coveredStatements", "totalStatements", "percentage", "files"})
	files, _ := covProject["files"].([]any)
	assertExactKeys(t, "coverage file", files[0], []string{"file", "coveredStatements", "totalStatements", "percentage"})
	builds, _ := doc["builds"].(map[string]any)
	tasks, _ := builds["tasks"].([]any)
	assertExactKeys(t, "build task", tasks[0], []string{"project", "task", "status", "durationMs", "cacheHit"})
}

func firstProject(t *testing.T, doc map[string]any, kind string) map[string]any {
	t.Helper()
	section, _ := doc[kind].(map[string]any)
	projects, _ := section["projects"].([]any)
	if len(projects) == 0 {
		t.Fatalf("%s payload has no projects: %v", kind, doc[kind])
	}
	project, _ := projects[0].(map[string]any)
	return project
}

func assertExactKeys(t *testing.T, label string, value any, want []string) {
	t.Helper()
	doc, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", label, value)
	}
	if len(doc) != len(want) {
		t.Fatalf("%s has keys %v, want exactly %v — keep the CLI twins in lockstep with the delivery API", label, doc, want)
	}
	for _, key := range want {
		if _, ok := doc[key]; !ok {
			t.Errorf("%s missing key %q — keep the CLI twins in lockstep with the delivery API", label, key)
		}
	}
}

func TestRepoKeyFromRemote(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
		err    bool
	}{
		{remote: "git@github.com:acme/app.git", want: "acme/app"},
		{remote: "https://github.com/acme/app.git", want: "acme/app"},
		{remote: "https://github.com/acme/app", want: "acme/app"},
		{remote: "ssh://git@github.com/acme/app.git", want: "acme/app"},
		{remote: "https://user:token@github.com/acme/app.git", want: "acme/app"},
		{remote: "https://ghe.example.com/org/sub/app.git", want: "sub/app"},
		{remote: "", err: true},
		{remote: "github.com", err: true},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			provider, key, err := clicore.RepositoryKeyFromRemote(tc.remote)
			if tc.err {
				if err == nil {
					t.Fatalf("want error, got %q", key)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepositoryKeyFromRemote: %v", err)
			}
			if provider != "github" {
				t.Fatalf("provider = %q, want github", provider)
			}
			if key != tc.want {
				t.Fatalf("repoKey = %q, want %q", key, tc.want)
			}
		})
	}
}

func TestResolveReportSubjectDetachedHeadDropsBranch(t *testing.T) {
	outs := cleanGitOutputs()
	outs["rev-parse --abbrev-ref HEAD"] = "HEAD"
	stubReportGit(t, &fakeGit{t: t, outs: outs})

	subject, err := resolveReportSubject(t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if subject.Branch != "" {
		t.Fatalf("branch = %q, want empty on detached HEAD", subject.Branch)
	}
	if subject.RepoKey != "acme/app" || subject.Provider != "github" {
		t.Fatalf("subject = %+v", subject)
	}
}

func TestReportHelpListsForms(t *testing.T) {
	var lines []string
	ioctx := clicore.IO{
		Env:    map[string]string{},
		Stdout: func(s string) { lines = append(lines, s) },
		Client: http.DefaultClient,
		Now:    time.Now,
	}
	if err := Report(map[string]any{}, []string{"help"}, "", ioctx.Env, ioctx); err != nil {
		t.Fatalf("help: %v", err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"--conclusion", "--exit-code", "-- <cmd"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("help output missing %q:\n%s", want, joined)
		}
	}
}

func writeReportTestAuth(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, clicore.AuthFileRelative)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeReportJSONFile(t, file, map[string]any{
		"access_token":  reportTokenResponse(nil)["access_token"],
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-06-30T13:00:00.000Z",
		"issuer":        reportTestBaseURL,
		"client_id":     "putnami-cli",
	})
}

func writeReportLinkFile(t *testing.T, workspaceRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeReportJSONFile(t, filepath.Join(workspaceRoot, clicore.LinkFileRelative), map[string]any{
		"version":           1,
		"control_plane_url": reportTestBaseURL,
		"workspace_id":      "ws-acme",
		"environment":       "prod",
	})
}

func reportTokenResponse(extra map[string]any) map[string]any {
	claims := map[string]any{
		"sub":   "user-1",
		"email": "dev@example.com",
		"scope": "openid profile email",
	}
	for k, v := range extra {
		claims[k] = v
	}
	return map[string]any{
		"access_token":  reportJWT(claims),
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_in":    300,
	}
}

func reportJWT(payload map[string]any) string {
	return reportBase64URL(map[string]any{"alg": "none", "typ": "JWT"}) + "." + reportBase64URL(payload) + ".sig"
}

func reportBase64URL(value map[string]any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func reportJSONResponse(status int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

func writeReportJSONFile(t *testing.T, file string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	if err := os.WriteFile(file, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}
