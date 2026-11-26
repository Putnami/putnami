package deliverycli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	pipelineFeature = "cloud/ci-pipelines"
	pipelineSHA     = "0123456789abcdef0123456789abcdef01234567"
	pipelinesPath   = "/v1/workspaces/ws-acme/ci/pipelines"
	providersPath   = "/v1/workspaces/ws-acme/ci/providers"
)

// ciFixture is one failed pipeline as Delivery answers it for one CI: the same
// members, in each CI's own words.
type ciFixture struct {
	provider, ref, repository, event, workflow string
	providerConclusion                         string
	change                                     int64
	capabilities                               []string
}

var (
	putnamiFixture = ciFixture{
		provider: "putnami-ci", ref: "putnami-ci:run-9", repository: "acme/app", event: "pull_request",
		workflow: "Putnami CI", providerConclusion: "failed", change: 7, capabilities: []string{"jobLogs", "steps"},
	}
	githubFixture = ciFixture{
		provider: "github-actions", ref: "github-actions:18273:2", repository: "acme/app", event: "pull_request",
		workflow: "Test", providerConclusion: "failure", change: 7, capabilities: []string{"jobLogs", "steps", "artifacts"},
	}
	gitlabFixture = ciFixture{
		provider: "gitlab-ci", ref: "gitlab-ci:5521", repository: "acme/platform/app", event: "merge_request_event",
		workflow: "", providerConclusion: "failed", change: 12, capabilities: []string{"jobLogs"},
	}
)

func (f ciFixture) job(id, name, conclusion string) map[string]any {
	return map[string]any{
		"ref": f.ref + ":" + id, "jobId": id, "attempt": 1, "name": name, "stage": "verify",
		"status": "completed", "conclusion": conclusion, "providerConclusion": f.ownWord(conclusion),
		"startedAt": "2026-07-24T08:01:00Z", "completedAt": "2026-07-24T08:03:05Z",
		"url": "https://ci.test/" + id,
	}
}

func (f ciFixture) ownWord(conclusion string) string {
	if conclusion == "failure" {
		return f.providerConclusion
	}
	return conclusion
}

func (f ciFixture) failedJob() map[string]any {
	job := f.job("501", "unit tests", "failure")
	job["failedStep"] = "Run tests"
	job["excerptState"] = "ready"
	return job
}

func (f ciFixture) pipeline(jobs ...map[string]any) map[string]any {
	doc := map[string]any{
		"ref": f.ref, "provider": f.provider, "pipelineId": "18273", "attempt": 2,
		"repository": f.repository, "headSha": pipelineSHA, "gitRef": "refs/heads/topic",
		"url": "https://ci.test/pipeline", "workflowName": f.workflow, "workflowPath": "ci/test.yml",
		"event": f.event, "change": f.change, "fork": true, "status": "completed", "conclusion": "failure",
		"providerStatus": "done", "providerConclusion": f.providerConclusion,
		"createdAt": "2026-07-24T08:00:00Z", "startedAt": "2026-07-24T08:00:30Z", "completedAt": "2026-07-24T08:04:30Z",
		"queueMs": 30000, "jobsCounted": len(jobs), "jobsKept": len(jobs), "indexedAt": "2026-07-24T08:05:00Z",
	}
	if len(jobs) > 0 {
		doc["jobs"] = jobs
	}
	return doc
}

func (f ciFixture) path() string               { return pipelinesPath + "/" + f.ref }
func (f ciFixture) jobPath(id string) string   { return f.path() + "/jobs/" + f.ref + ":" + id }
func (f ciFixture) failedStep() map[string]any { return ciStepDoc(3, "Run tests", "failure") }
func (f ciFixture) excerpt() map[string]any {
	return ciExcerptDoc("--- FAIL: TestLogin\nwant 200, got 500", 88, 89)
}
func (f ciFixture) reads(jobs ...map[string]any) map[string]ciStubResponse {
	return map[string]ciStubResponse{
		"GET " + f.path(): {status: http.StatusOK, body: map[string]any{"pipeline": f.pipeline(jobs...), "capabilities": f.capabilities}},
	}
}

func ciStepDoc(number int, name, conclusion string) map[string]any {
	return map[string]any{"number": number, "name": name, "status": "completed", "conclusion": conclusion}
}

func ciExcerptDoc(text string, from, to int) map[string]any {
	return map[string]any{"text": text, "bytes": len(text), "lines": to - from + 1, "fromLine": from, "toLine": to}
}

// ciPipelineRun runs one command and keeps both outputs.
type ciPipelineRun struct {
	stdout, stderr []string
	err            error
}

func (r ciPipelineRun) out() string  { return strings.Join(r.stdout, "\n") }
func (r ciPipelineRun) errs() string { return strings.Join(r.stderr, "\n") }

func (r ciPipelineRun) wants(t *testing.T, wants ...string) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("command failed: %v\nstderr: %s", r.err, r.errs())
	}
	for _, want := range wants {
		if !strings.Contains(r.out(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.out())
		}
	}
}

func (r ciPipelineRun) refuses(t *testing.T, code int, wants ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("command succeeded, want a refusal:\n%s", r.out())
	}
	if got := clicore.ExitCode(r.err); got != code {
		t.Errorf("exit code = %d, want %d (%v)", got, code, r.err)
	}
	for _, want := range wants {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, r.err)
		}
	}
}

func runPipelineCI(t *testing.T, fake *ciFakeServer, args ...string) ciPipelineRun {
	t.Helper()
	ioctx, root := newCITestIO(t, fake)
	return runPipelineCIIn(ioctx, root, args...)
}

func runPipelineCIIn(ioctx clicore.IO, root string, args ...string) ciPipelineRun {
	var run ciPipelineRun
	ioctx.Stdout = func(line string) { run.stdout = append(run.stdout, line) }
	ioctx.Stderr = func(line string) { run.stderr = append(run.stderr, line) }
	run.err = runCI(ioctx, root, args)
	return run
}

// stubPipelineGit names the repository state, one output per command.
func stubPipelineGit(t *testing.T, outs map[string]string) {
	t.Helper()
	previous := ciPipelineGit
	ciPipelineGit = func(_ string, args ...string) (string, error) {
		if out, ok := outs[strings.Join(args, " ")]; ok {
			return out, nil
		}
		return "", errors.New("git: no such answer")
	}
	t.Cleanup(func() { ciPipelineGit = previous })
}

// ciReads keeps the requests a command sent to the neutral routes.
func ciReads(fake *ciFakeServer) []ciCapturedRequest {
	var reads []ciCapturedRequest
	for _, request := range fake.requests {
		if strings.Contains(request.Path, "/ci/") {
			reads = append(reads, request)
		}
	}
	return reads
}

// --- list ---

func ciListsOnePipeline(t *testing.T, fixture ciFixture) {
	t.Helper()
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + pipelinesPath: {status: http.StatusOK, body: map[string]any{"pipelines": []any{fixture.pipeline()}}},
	}}
	run := runPipelineCI(t, fake, "list", "--change", "!"+fmt.Sprint(fixture.change), "--repo", fixture.repository)
	run.wants(t, "CI ", "WORKFLOW", "STATE", "PIPELINE", fixture.provider, fixture.ref, "failure",
		fixture.event+" #"+fmt.Sprint(fixture.change)+" (fork)", "putnami cloud ci view --pipeline <ref>")
	reads := ciReads(fake)
	if len(reads) != 1 || reads[0].Path != pipelinesPath {
		t.Fatalf("reads = %+v, want the pipelines route alone", reads)
	}
	query, _ := url.ParseQuery(reads[0].Query)
	want := url.Values{"change": {fmt.Sprint(fixture.change)}, "repository": {fixture.repository}}
	if !reflect.DeepEqual(query, want) {
		t.Errorf("query = %v, want %v", query, want)
	}
}

func TestCIPipelineListReadsPutnamiCI(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-reads-a-putnami-ci-pipeline")
	ciListsOnePipeline(t, putnamiFixture)
}

func TestCIPipelineListReadsGitHubActions(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-reads-a-github-actions-pipeline")
	ciListsOnePipeline(t, githubFixture)
}

func TestCIPipelineListReadsGitLabCI(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-reads-a-gitlab-ci-pipeline")
	ciListsOnePipeline(t, gitlabFixture)
}

func TestCIPipelineListResolvesTheCommitAndTheRepository(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-resolves-the-commit-and-the-origin-repository")
	const verify = "rev-parse --verify --quiet "
	for _, remote := range []string{"git@gitlab.example:acme/platform/app.git", "https://user:secret@gitlab.example/acme/platform/app"} {
		stubPipelineGit(t, map[string]string{verify + "HEAD^{commit}": pipelineSHA, "remote get-url origin": remote})
		fake := &ciFakeServer{responses: map[string]ciStubResponse{
			"GET " + pipelinesPath: {status: http.StatusOK, body: map[string]any{"pipelines": []any{gitlabFixture.pipeline()}, "truncated": true}},
		}}
		run := runPipelineCI(t, fake, "list", "--commit", "HEAD", "--limit", "1")
		run.wants(t, "older pipelines were left out")
		query, _ := url.ParseQuery(ciReads(fake)[0].Query)
		want := url.Values{"commit": {pipelineSHA}, "repository": {"acme/platform/app"}, "limit": {"1"}}
		if !reflect.DeepEqual(query, want) {
			t.Errorf("query = %v, want %v", query, want)
		}
	}

	// A full id needs no Git object: a shallow clone does not hold it.
	stubPipelineGit(t, nil)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + pipelinesPath: {status: http.StatusOK, body: map[string]any{"pipelines": []any{githubFixture.pipeline()}}},
	}}
	runPipelineCI(t, fake, "list", "--commit", strings.ToUpper(pipelineSHA), "--repo", "acme/app").wants(t, githubFixture.ref)

	for name, refusal := range map[string]struct {
		args  []string
		wants []string
	}{
		"unknown revision": {[]string{"list", "--commit", "nope", "--repo", "acme/app"}, []string{"--commit nope is no commit"}},
		"an option":        {[]string{"list", "--commit=--all", "--repo", "acme/app"}, []string{"--commit is not a revision"}},
		"no remote":        {[]string{"list", "--change", "7"}, []string{"pass --repo"}},
		"both":             {[]string{"list", "--change", "7", "--commit", pipelineSHA}, []string{"not both"}},
		"change":           {[]string{"list", "--change", "seven"}, []string{"--change must be the number"}},
		"limit":            {[]string{"list", "--change", "7", "--limit", "101"}, []string{"--limit must be 1 to 100"}},
		"run filter":       {[]string{"list", "--change", "7", "--branch", "main"}, []string{"--branch does not apply"}},
		"job":              {[]string{"list", "--change", "7", "--job", "lint"}, []string{"--job does not apply"}},
		"raw":              {[]string{"list", "--change", "7", "--raw"}, []string{"--raw does not apply"}},
		"positional":       {[]string{"list", "run-9", "--change", "7"}, []string{"takes no positional"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &ciFakeServer{}
			runPipelineCI(t, fake, refusal.args...).refuses(t, clicore.ExitUsage, refusal.wants...)
			if reads := ciReads(fake); len(reads) != 0 {
				t.Errorf("a refused command read %+v", reads)
			}
		})
	}
}

func TestCIRepositoryFromRemote(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:acme/app.git":             "acme/app",
		"https://github.com/acme/app":             "acme/app",
		"ssh://git@gitlab.example:2222/a/b/c.git": "a/b/c",
		"https://token@gitlab.example/a/b/c.git/": "a/b/c",
		"gitlab.example:a/b/c":                    "a/b/c",
		"/srv/git/app.git":                        "",
		"../app":                                  "",
		"file:///srv/git/app.git":                 "",
		"https://github.com/app":                  "",
		"git@github.com:":                         "",
		"./host:acme/app":                         "",
		"https://github.com/acme//app":            "",
	} {
		if got := ciRepositoryFromRemote(remote); got != want {
			t.Errorf("ciRepositoryFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestCIPipelineListRefusesALocalRemoteWithoutPrintingIt(t *testing.T) {
	stubPipelineGit(t, map[string]string{"remote get-url origin": "https://user:hunter2@github.com/app"})
	run := runPipelineCI(t, &ciFakeServer{}, "list", "--change", "7")
	run.refuses(t, clicore.ExitUsage, "pass --repo")
	if strings.Contains(run.err.Error()+run.errs(), "hunter2") {
		t.Errorf("the refusal prints the remote: %v", run.err)
	}
}

func TestCIPipelineListJSONKeepsEveryMember(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-json-keeps-every-pipeline-member")
	job := githubFixture.failedJob()
	job["providerStatus"], job["allowedFailure"], job["retried"] = "done", true, true
	job["queueMs"], job["runnerClass"], job["excerptReason"] = 1200, "linux-4", "expired"
	want := []map[string]any{githubFixture.pipeline(job), gitlabFixture.pipeline(gitlabFixture.failedJob())}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + pipelinesPath: {status: http.StatusOK, body: map[string]any{"pipelines": want, "truncated": true}},
	}}
	run := runPipelineCI(t, fake, "list", "--change", "7", "--repo", "acme/app", "--output=json")
	run.wants(t)
	if len(run.stdout) != len(want) {
		t.Fatalf("stdout = %d lines, want one per pipeline:\n%s", len(run.stdout), run.out())
	}
	for i, line := range run.stdout {
		expected, _ := json.Marshal(want[i])
		var got, doc any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is no JSON: %v", i, err)
		}
		_ = json.Unmarshal(expected, &doc)
		if members, ok := doc.(map[string]any); ok && members["workflowName"] == "" {
			delete(members, "workflowName")
		}
		if !reflect.DeepEqual(got, doc) {
			t.Errorf("line %d = %s\nwant %s", i, line, expected)
		}
	}
	if !strings.Contains(run.errs(), "older pipelines were left out") {
		t.Errorf("stderr lacks the truncation note: %s", run.errs())
	}
}

func TestCIListWithoutACommitOrChangeListsHostedRuns(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-without-a-commit-or-change-lists-hosted-runs")
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: http.StatusOK, body: map[string]any{"runs": []any{}}},
	}}
	run := runPipelineCI(t, fake, "list")
	run.wants(t)
	if reads := ciReads(fake); len(reads) != 0 {
		t.Errorf("list read the neutral routes: %+v", reads)
	}
	if !slices.ContainsFunc(fake.requests, func(r ciCapturedRequest) bool { return r.Path == "/v1/workspaces/ws-acme/runs" }) {
		t.Errorf("requests = %+v, want the runs route", fake.requests)
	}
}

// --- providers ---

func providersStub() ciStubResponse {
	return ciStubResponse{status: http.StatusOK, body: map[string]any{"providers": []any{
		map[string]any{"provider": "putnami-ci", "state": "disabled"},
		map[string]any{"provider": "github-actions", "state": "not_granted", "reason": "the Actions permission is missing"},
		map[string]any{"provider": "gitlab-ci", "state": "connected", "lastSeen": "2026-07-24T08:00:00Z", "capabilities": []string{"jobLogs", "artifacts"}},
	}}}
}

func TestCIProvidersPrintEachState(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "says-which-ci-cloud-reads", "providers-print-each-ci-state")
	fake := &ciFakeServer{responses: map[string]ciStubResponse{"GET " + providersPath: providersStub()}}
	run := runPipelineCI(t, fake, "providers")
	run.wants(t, "putnami-ci", "disabled", "github-actions", "not_granted", "the Actions permission is missing",
		"gitlab-ci", "connected", "jobLogs,artifacts")

	run = runPipelineCI(t, fake, "providers", "--output=json")
	run.wants(t, `"provider":"github-actions"`, `"reason":"the Actions permission is missing"`)
	if len(run.stdout) != 3 {
		t.Errorf("stdout = %d lines, want one per CI:\n%s", len(run.stdout), run.out())
	}

	fake = &ciFakeServer{responses: map[string]ciStubResponse{"GET " + providersPath: {status: http.StatusOK, body: map[string]any{}}}}
	runPipelineCI(t, fake, "providers").wants(t, "Cloud reads no CI for this workspace.")

	// A control plane without the routes is not an unknown run.
	fake = &ciFakeServer{responses: map[string]ciStubResponse{"GET " + providersPath: {status: http.StatusNotFound, body: map[string]any{"error": "no such route"}}}}
	run = runPipelineCI(t, fake, "providers")
	run.refuses(t, clicore.ExitAPI, "no such route", "Cloud does not serve the reads of any CI to this workspace")
}

func TestCIPipelineEmptyListSaysWhichCICloudReads(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "says-which-ci-cloud-reads", "an-empty-list-says-which-ci-cloud-reads")
	empty := ciStubResponse{status: http.StatusOK, body: map[string]any{"pipelines": []any{}}}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{"GET " + pipelinesPath: empty, "GET " + providersPath: providersStub()}}
	args := []string{"list", "--commit", pipelineSHA, "--repo", "acme/app"}
	runPipelineCI(t, fake, args...).wants(t, "No pipeline found for commit 0123456789ab of acme/app.",
		"github-actions: not_granted (the Actions permission is missing)", "putnami-ci: disabled")

	run := runPipelineCI(t, fake, append(args, "--output=json")...)
	run.wants(t)
	if len(run.stdout) != 0 || !strings.Contains(run.errs(), "github-actions: not_granted") {
		t.Errorf("structured output: stdout %q, stderr %q; want the note on stderr alone", run.out(), run.errs())
	}

	delete(fake.responses, "GET "+providersPath)
	runPipelineCI(t, fake, args...).wants(t, "The state of each CI could not be read")
}

// A full page at the bound cannot be raised, so the note does not say to.
func TestCIPipelineListAtTheBoundDoesNotSayToRaiseIt(t *testing.T) {
	pipelines := make([]any, ciPipelineListMax)
	for i := range pipelines {
		pipelines[i] = githubFixture.pipeline()
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + pipelinesPath: {status: http.StatusOK, body: map[string]any{"pipelines": pipelines, "truncated": true}},
	}}
	run := runPipelineCI(t, fake, "list", "--change", "7", "--repo", "acme/app", "--limit", "100")
	run.wants(t, "100 is the most one list reads")
	if strings.Contains(run.out(), "raise --limit") {
		t.Errorf("the note says to raise a limit that is at its bound:\n%s", run.out())
	}
}

// cancelOn cancels the command's context when the path is read.
type cancelOn struct {
	base   http.RoundTripper
	path   string
	cancel context.CancelFunc
}

func (c cancelOn) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != c.path {
		return c.base.RoundTrip(req)
	}
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	_ = resp.Body.Close()
	c.cancel()
	resp.Body = io.NopCloser(canceledBody{})
	return resp, nil
}

type canceledBody struct{}

func (canceledBody) Read([]byte) (int, error) { return 0, context.Canceled }

func TestCIPipelineReadsSayTheyWereCanceled(t *testing.T) {
	for name, c := range map[string]struct {
		path string
		args []string
	}{
		"log":       {githubFixture.jobPath("501") + "/log", []string{"logs", "--pipeline", githubFixture.ref}},
		"providers": {providersPath, []string{"list", "--change", "7", "--repo", "acme/app"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &ciFakeServer{responses: githubFixture.reads(githubFixture.failedJob())}
			fake.responses["GET "+pipelinesPath] = ciStubResponse{status: http.StatusOK, body: map[string]any{}}
			fake.responses["GET "+providersPath] = providersStub()
			fake.responses["GET "+githubFixture.jobPath("501")+"/log"] = ciStubResponse{status: http.StatusOK, text: "the log\n"}
			ioctx, root := newCITestIO(t, fake)
			ctx, cancel := context.WithCancel(t.Context())
			ioctx.Context = ctx
			ioctx.Client = &http.Client{Transport: cancelOn{base: fake, path: c.path, cancel: cancel}}
			runPipelineCIIn(ioctx, root, c.args...).refuses(t, clicore.ExitUsage, "canceled by user")
		})
	}
}

// --- view ---

func ciExplainsOneFailure(t *testing.T, fixture ciFixture) {
	t.Helper()
	fake := &ciFakeServer{responses: fixture.reads(fixture.job("500", "lint", "success"), fixture.failedJob())}
	fake.responses["GET "+fixture.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{
		"pipeline": fixture.pipeline(), "job": fixture.failedJob(),
		"steps":   []any{ciStepDoc(1, "Set up", "success"), fixture.failedStep()},
		"excerpt": fixture.excerpt(),
	}}
	run := runPipelineCI(t, fake, "view", "--pipeline", fixture.ref)
	run.wants(t, fixture.provider, fixture.repository, pipelineSHA, "Job unit tests: failure", "failed step 3: Run tests",
		"log, lines 88 to 89:", "    --- FAIL: TestLogin", "    want 200, got 500",
		"full log: putnami cloud ci logs --pipeline "+fixture.ref+" --job 501")
	if strings.Contains(run.out(), "Set up") {
		t.Errorf("view prints a step that passed:\n%s", run.out())
	}
	if failed, passed := strings.Index(run.out(), "unit tests"), strings.Index(run.out(), "lint"); failed > passed {
		t.Errorf("the passed job comes before the failed one:\n%s", run.out())
	}
	paths := []string{}
	for _, read := range ciReads(fake) {
		paths = append(paths, read.Path)
	}
	if want := []string{fixture.path(), fixture.jobPath("501")}; !slices.Equal(paths, want) {
		t.Errorf("reads = %v, want %v", paths, want)
	}
}

func TestCIPipelineViewExplainsAPutnamiCIFailure(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-explains-a-putnami-ci-failure")
	ciExplainsOneFailure(t, putnamiFixture)
}

func TestCIPipelineViewExplainsAGitHubActionsFailure(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-explains-a-github-actions-failure")
	ciExplainsOneFailure(t, githubFixture)
}

func TestCIPipelineViewExplainsAGitLabCIFailure(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-explains-a-gitlab-ci-failure")
	ciExplainsOneFailure(t, gitlabFixture)
}

func TestCIPipelineViewLeavesOutAllowedAndRetriedJobs(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-leaves-out-allowed-and-retried-jobs")
	allowed := gitlabFixture.job("601", "canary", "failure")
	allowed["allowedFailure"] = true
	retried := gitlabFixture.job("602", "flaky", "failure")
	retried["retried"] = true
	fake := &ciFakeServer{responses: gitlabFixture.reads(allowed, retried, gitlabFixture.job("603", "flaky", "success"))}
	run := runPipelineCI(t, fake, "view", "--pipeline", gitlabFixture.ref)
	run.wants(t, "failure (failed), allowed", "failure (failed), retried", "No job Cloud kept explains it: the CI says failed.")
	if strings.Contains(run.out(), "Job canary") || strings.Contains(run.out(), "Job flaky") {
		t.Errorf("view explains a job that does not fail the pipeline:\n%s", run.out())
	}
	if reads := ciReads(fake); len(reads) != 1 {
		t.Errorf("reads = %+v, want the pipeline alone", reads)
	}

	// More failed jobs than view explains: the rest are counted, never hidden.
	jobs := []map[string]any{}
	fake = &ciFakeServer{}
	for i := range ciPipelineFailureLimit + 2 {
		jobs = append(jobs, githubFixture.job(fmt.Sprint(700+i), fmt.Sprintf("shard %d", i), "timed_out"))
	}
	fake.responses = githubFixture.reads(jobs...)
	for i, job := range jobs[:ciPipelineFailureLimit] {
		if i == 1 {
			continue // Its read fails: the job is still named.
		}
		fake.responses["GET "+githubFixture.jobPath(fmt.Sprint(700+i))] = ciStubResponse{status: http.StatusOK, body: map[string]any{"job": job}}
	}
	run = runPipelineCI(t, fake, "view", "--pipeline", githubFixture.ref)
	run.wants(t, "Job shard 1: timed_out", "Job shard 4: timed_out", "… 2 more failed jobs; read one with --job <id>.")
	if !strings.Contains(run.errs(), "job shard 1 was not read whole") {
		t.Errorf("stderr lacks the unread job: %s", run.errs())
	}
	if got := len(ciReads(fake)); got != 1+ciPipelineFailureLimit {
		t.Errorf("%d reads, want %d", got, 1+ciPipelineFailureLimit)
	}
}

func TestCIPipelineViewExplainsAMissingExcerpt(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-explains-a-missing-excerpt")
	for _, c := range []struct{ state, reason, want string }{
		{"pending", "", "log excerpt: not read yet; run the command again in a minute"},
		{"unavailable", "the log expired", "log excerpt: not available (the log expired)"},
		{"unavailable", "", "log excerpt: not available (no reason given)"},
		{"", "", "log excerpt: none kept"},
	} {
		job := githubFixture.failedJob()
		job["excerptState"], job["excerptReason"] = c.state, c.reason
		fake := &ciFakeServer{responses: githubFixture.reads(job)}
		fake.responses["GET "+githubFixture.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{"job": job}}
		runPipelineCI(t, fake, "view", "--pipeline", githubFixture.ref).wants(t, c.want, "failed step: Run tests")
	}

	// A CI that offers no log gives the job's page, not a command that fails.
	noLogs := githubFixture
	noLogs.capabilities = []string{"steps"}
	fake := &ciFakeServer{responses: noLogs.reads(noLogs.failedJob())}
	fake.responses["GET "+noLogs.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{"job": noLogs.failedJob()}}
	run := runPipelineCI(t, fake, "view", "--pipeline", noLogs.ref)
	run.wants(t, "page: https://ci.test/501")
	if strings.Contains(run.out(), "full log:") {
		t.Errorf("view names a log this CI does not offer:\n%s", run.out())
	}
}

func TestCIPipelineViewSelectsOneJob(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-selects-one-job")
	passed := githubFixture.job("500", "lint", "success")
	twin := githubFixture.job("502", "unit tests", "success")
	reads := func() *ciFakeServer {
		fake := &ciFakeServer{responses: githubFixture.reads(passed, githubFixture.failedJob(), twin)}
		fake.responses["GET "+githubFixture.jobPath("500")] = ciStubResponse{status: http.StatusOK, body: map[string]any{"job": passed}}
		return fake
	}
	for _, selector := range []string{"500", "lint", githubFixture.ref + ":500"} {
		run := runPipelineCI(t, reads(), "view", "--pipeline", githubFixture.ref, "--job", selector)
		run.wants(t, "Job lint: success")
		if strings.Contains(run.out(), "unit tests") {
			t.Errorf("--job %s prints another job:\n%s", selector, run.out())
		}
	}
	runPipelineCI(t, reads(), "view", "--pipeline", githubFixture.ref, "--job", "unit tests").
		refuses(t, clicore.ExitUsage, "2 jobs are named unit tests; pass --job <id>:", "501  failure  unit tests", "502  success  unit tests")
	runPipelineCI(t, reads(), "view", "--pipeline", githubFixture.ref, "--job", "deploy").
		refuses(t, clicore.ExitUsage, "no job is named deploy", "500  success  lint")
	runPipelineCI(t, reads(), "view", "run-9", "--pipeline", githubFixture.ref).
		refuses(t, clicore.ExitUsage, "pass a run id or --pipeline, not both")
	runPipelineCI(t, reads(), "view", "--pipeline", githubFixture.ref, "--raw").refuses(t, clicore.ExitUsage, "--raw does not apply")
	runPipelineCI(t, reads(), "view", "run-9", "--job", "lint").refuses(t, clicore.ExitUsage, "--job does not apply")
	runPipelineCI(t, &ciFakeServer{}, "view", "--pipeline", "nope").
		refuses(t, clicore.ExitAPI, "404 — this workspace has no such pipeline or job")
}

func TestCIPipelineViewJSONCarriesTheFailures(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "shows-why-a-pipeline-failed", "view-json-carries-the-failures")
	fake := &ciFakeServer{responses: gitlabFixture.reads(gitlabFixture.failedJob())}
	fake.responses["GET "+gitlabFixture.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{
		"pipeline": gitlabFixture.pipeline(), "job": gitlabFixture.failedJob(), "steps": []any{gitlabFixture.failedStep()}, "excerpt": gitlabFixture.excerpt(),
	}}
	run := runPipelineCI(t, fake, "view", "--pipeline", gitlabFixture.ref, "--output=json")
	run.wants(t)
	var answer CIPipelineFailures
	if err := json.Unmarshal([]byte(run.out()), &answer); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, run.out())
	}
	if answer.Pipeline.Ref != gitlabFixture.ref || len(answer.Pipeline.Jobs) != 1 || !slices.Equal(answer.Capabilities, gitlabFixture.capabilities) {
		t.Errorf("answer = %+v, want the pipeline, its jobs and its capabilities", answer)
	}
	if len(answer.Failures) != 1 || answer.Failures[0].Job.JobID != "501" || len(answer.Failures[0].Steps) != 1 ||
		answer.Failures[0].Excerpt == nil || answer.Failures[0].Excerpt.FromLine != 88 || answer.OmittedFailures != 0 {
		t.Errorf("failures = %+v, want job 501 with its step and excerpt", answer.Failures)
	}

	run = runPipelineCI(t, fake, "view", "--pipeline", gitlabFixture.ref, "--job", "501", "--output=json")
	run.wants(t)
	var one CIPipelineJobDetail
	if err := json.Unmarshal([]byte(run.out()), &one); err != nil || one.Job.JobID != "501" || one.Excerpt == nil {
		t.Errorf("--job answer = %+v (%v), want job 501 with its excerpt", one, err)
	}

	passed := &ciFakeServer{responses: gitlabFixture.reads(gitlabFixture.job("500", "lint", "success"))}
	runPipelineCI(t, passed, "view", "--pipeline", gitlabFixture.ref, "--output=json").wants(t, `"failures":[]`, `"omittedFailures":0`)
}

// --- logs ---

func ciReadsOneLog(t *testing.T, fixture ciFixture) {
	t.Helper()
	fake := &ciFakeServer{responses: fixture.reads(fixture.job("500", "lint", "success"), fixture.failedJob())}
	fake.responses["GET "+fixture.jobPath("500")+"/log"] = ciStubResponse{status: http.StatusOK, text: "first\r\nsecond\n"}
	run := runPipelineCI(t, fake, "logs", "--pipeline", fixture.ref, "--job", "lint")
	run.wants(t)
	if !slices.Equal(run.stdout, []string{"first", "second"}) || len(run.stderr) != 0 {
		t.Errorf("stdout = %q, stderr = %q, want the two lines alone", run.stdout, run.stderr)
	}
	reads := ciReads(fake)
	if last := reads[len(reads)-1]; last.Path != fixture.jobPath("500")+"/log" || last.Auth == "" {
		t.Errorf("last read = %+v, want the signed-in log read", last)
	}

	run = runPipelineCI(t, fake, "logs", "--pipeline", fixture.ref, "--job", "lint", "--output=json")
	run.wants(t)
	if !slices.Equal(run.stdout, []string{`{"line":1,"text":"first"}`, `{"line":2,"text":"second"}`}) {
		t.Errorf("structured stdout = %q", run.stdout)
	}
}

func TestCIPipelineLogsReadAPutnamiCIJob(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-read-a-putnami-ci-job")
	ciReadsOneLog(t, putnamiFixture)
}

func TestCIPipelineLogsReadAGitHubActionsJob(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-read-a-github-actions-job")
	ciReadsOneLog(t, githubFixture)
}

func TestCIPipelineLogsReadAGitLabCIJob(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-read-a-gitlab-ci-job")
	ciReadsOneLog(t, gitlabFixture)
}

func TestCIPipelineLogsPickTheFailedJob(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-pick-the-failed-job")
	logOf := func(id string) ciStubResponse {
		return ciStubResponse{status: http.StatusOK, text: "log of " + id + "\n"}
	}
	allowed := githubFixture.job("502", "canary", "failure")
	allowed["allowedFailure"] = true
	fake := &ciFakeServer{responses: githubFixture.reads(githubFixture.job("500", "lint", "success"), allowed, githubFixture.failedJob())}
	fake.responses["GET "+githubFixture.jobPath("501")+"/log"] = logOf("501")
	runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref).wants(t, "log of 501")

	fake = &ciFakeServer{responses: githubFixture.reads(githubFixture.job("500", "lint", "success"))}
	fake.responses["GET "+githubFixture.jobPath("500")+"/log"] = logOf("500")
	runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref).wants(t, "log of 500")

	fake = &ciFakeServer{responses: githubFixture.reads(githubFixture.job("500", "lint", "success"), githubFixture.failedJob(), githubFixture.job("503", "e2e", "failure"))}
	runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref).
		refuses(t, clicore.ExitUsage, "the pipeline has several jobs; pass --job <id>:\n  501", "503  failure  e2e", "500  success  lint")
	runPipelineCI(t, &ciFakeServer{responses: githubFixture.reads()}, "logs", "--pipeline", githubFixture.ref).
		refuses(t, clicore.ExitUsage, "the pipeline has no job yet")
}

func TestCIPipelineLogsSayTheLogIsCut(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-say-the-log-is-cut")
	fake := &ciFakeServer{responses: putnamiFixture.reads(putnamiFixture.failedJob())}
	fake.responses["GET "+putnamiFixture.jobPath("501")+"/log"] = ciStubResponse{
		status: http.StatusOK, text: "the end\n", header: http.Header{"Putnami-Ci-Log-Tail": {"true"}},
	}
	run := runPipelineCI(t, fake, "logs", "--pipeline", putnamiFixture.ref, "--output=json")
	run.wants(t)
	if len(run.stdout) != 1 || !strings.Contains(run.errs(), "this is the end of the log only") {
		t.Errorf("stdout = %q, stderr = %q, want the note on stderr", run.stdout, run.stderr)
	}

	fake.responses["GET "+putnamiFixture.jobPath("501")+"/log"] = ciStubResponse{status: http.StatusOK, header: http.Header{}}
	run = runPipelineCI(t, fake, "logs", "--pipeline", putnamiFixture.ref)
	run.wants(t)
	if len(run.stdout) != 0 || !strings.Contains(run.errs(), "the log of unit tests is empty") {
		t.Errorf("stdout = %q, stderr = %q, want the empty log said", run.stdout, run.stderr)
	}
}

func TestCIPipelineLogsNameTheUnsupportedRead(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-name-the-unsupported-read")
	for _, c := range []struct {
		status int
		reason string
		wants  []string
	}{
		{http.StatusUnprocessableEntity, "gitlab-ci does not offer jobLogs", []string{"cloud ci logs: gitlab-ci does not offer jobLogs"}},
		{http.StatusTooManyRequests, "rate limited", []string{"rate limited", "429 — the CI limits how often it is read"}},
		{http.StatusNotFound, "no such job", []string{"no such job", "404 — this workspace has no such pipeline or job"}},
		{http.StatusForbidden, "machine tokens are denied", []string{"machine tokens are denied"}},
		{http.StatusBadGateway, "the CI did not answer", []string{"the CI did not answer"}},
	} {
		fake := &ciFakeServer{responses: gitlabFixture.reads(gitlabFixture.failedJob())}
		fake.responses["GET "+gitlabFixture.jobPath("501")+"/log"] = ciStubResponse{status: c.status, body: map[string]any{"error": c.reason}}
		run := runPipelineCI(t, fake, "logs", "--pipeline", gitlabFixture.ref)
		run.refuses(t, clicore.ExitAPI, c.wants...)
		if !strings.Contains(run.errs(), "the job's page is https://ci.test/501") {
			t.Errorf("%d: stderr lacks the job's page: %s", c.status, run.errs())
		}
	}

	// A log past the read bound is refused, never printed cut without a word.
	fake := &ciFakeServer{responses: gitlabFixture.reads(gitlabFixture.failedJob())}
	fake.responses["GET "+gitlabFixture.jobPath("501")+"/log"] = ciStubResponse{status: http.StatusOK, text: strings.Repeat("0123456789abcde\n", (33<<20)/16)}
	run := runPipelineCI(t, fake, "logs", "--pipeline", gitlabFixture.ref)
	run.refuses(t, clicore.ExitAPI, "the log was not read whole")
	if len(run.stdout) != 0 || !strings.Contains(run.errs(), "the job's page is") {
		t.Errorf("%d lines printed, stderr = %s; want the page alone", len(run.stdout), run.errs())
	}
}

func TestCIPipelineLogsPrintNoTerminalControlSequence(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-print-no-terminal-control-sequence")
	const hostile = "\x1b[31mred\x1b[0m\x1b]52;c;cGF5bG9hZA==\x07\x1b]8;;https://evil.test\x1b\\link\u202eedoc\x08\x9b2J\tend"
	fake := &ciFakeServer{responses: githubFixture.reads(githubFixture.failedJob())}
	fake.responses["GET "+githubFixture.jobPath("501")+"/log"] = ciStubResponse{status: http.StatusOK, text: hostile + "\n"}
	run := runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref)
	run.wants(t, "redlinkedoc")
	if strings.ContainsAny(run.out(), "\x1b\x07\x08\u202e\u009b") || strings.Contains(run.out(), "evil.test") || strings.Contains(run.out(), "cGF5") {
		t.Errorf("stdout keeps a control sequence: %q", run.out())
	}
	raw := runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref, "--raw")
	raw.wants(t, "\x1b[31mred\x1b[0m")

	// The names a change author writes are no safer than its logs.
	job := githubFixture.failedJob()
	job["name"] = "tests\x1b[2J\x1b]0;owned\x07\n\u202ehidden"
	fake = &ciFakeServer{responses: githubFixture.reads(job)}
	fake.responses["GET "+githubFixture.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{
		"job": job, "excerpt": ciExcerptDoc("boom\x1b[2J", 1, 1), "steps": []any{ciStepDoc(1, "step\x1b[1m", "failure")},
	}}
	run = runPipelineCI(t, fake, "view", "--pipeline", githubFixture.ref)
	run.wants(t, "Job tests hidden: failure", "failed step 1: step", "    boom")
	if strings.ContainsAny(run.out(), "\x1b\x07\u202e") {
		t.Errorf("view keeps a control sequence: %q", run.out())
	}
}

func TestCIPipelineLogsRefuseRunOnlyFlags(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-a-job-log", "logs-refuse-run-only-flags")
	for _, flag := range []string{"--follow", "--phase=test", "--project=a", "--q=x", "--level=error", "--from=1", "--to=2", "--cursor=c", "--limit=5"} {
		fake := &ciFakeServer{}
		name, _, _ := strings.Cut(flag, "=")
		runPipelineCI(t, fake, "logs", "--pipeline", githubFixture.ref, flag).refuses(t, clicore.ExitUsage, name+" does not apply with --pipeline")
		if reads := ciReads(fake); len(reads) != 0 {
			t.Errorf("%s: a refused command read %+v", flag, reads)
		}
	}
	runPipelineCI(t, &ciFakeServer{}, "logs", "run-9", "--pipeline", githubFixture.ref).refuses(t, clicore.ExitUsage, "not both")
	runPipelineCI(t, &ciFakeServer{}, "logs", "run-9", "--job", "lint").refuses(t, clicore.ExitUsage, "--job does not apply")
}

// --- outside a workspace ---

// unlinkedCIIO is a signed-in person in a directory with no Putnami file.
func unlinkedCIIO(t *testing.T, fake *ciFakeServer) (clicore.IO, string) {
	t.Helper()
	home := t.TempDir()
	writeReportTestAuth(t, home)
	ioctx, _ := newCITestIO(t, fake)
	ioctx.Env = hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": reportTestBaseURL, "NO_COLOR": "1"})
	return ioctx, t.TempDir()
}

func TestCIPipelineReadsRunWithoutAWorkspace(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-outside-a-workspace", "pipeline-reads-run-without-a-workspace")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		CommandGroups map[string]struct {
			Subcommands map[string]struct {
				Workspace string `json:"workspace"`
			} `json:"subcommands"`
		} `json:"commandGroups"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	if got := manifest.CommandGroups["cloud"].Subcommands["ci"].Workspace; got != "optional" {
		t.Errorf("cloud ci workspace = %q, want optional", got)
	}

	stubPipelineGit(t, map[string]string{"remote get-url origin": "git@github.com:acme/app.git"})
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"list", []string{"list", "--change", "7"}, githubFixture.ref},
		{"providers", []string{"providers"}, "github-actions"},
		{"view", []string{"view", "--pipeline", githubFixture.ref}, "Job unit tests: failure"},
		{"logs", []string{"logs", "--pipeline", githubFixture.ref}, "the log"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &ciFakeServer{responses: githubFixture.reads(githubFixture.failedJob())}
			fake.responses["GET "+pipelinesPath] = ciStubResponse{status: http.StatusOK, body: map[string]any{"pipelines": []any{githubFixture.pipeline()}}}
			fake.responses["GET "+providersPath] = providersStub()
			fake.responses["GET /v1/workspaces"] = ciStubResponse{status: http.StatusOK, body: map[string]any{"workspaces": []any{map[string]any{"id": "ws-acme", "slug": "acme"}}}}
			fake.responses["GET "+githubFixture.jobPath("501")] = ciStubResponse{status: http.StatusOK, body: map[string]any{"job": githubFixture.failedJob()}}
			fake.responses["GET "+githubFixture.jobPath("501")+"/log"] = ciStubResponse{status: http.StatusOK, text: "the log\n"}
			ioctx, root := unlinkedCIIO(t, fake)
			runPipelineCIIn(ioctx, root, append(c.args, "--workspace", "acme")...).wants(t, c.want)
			def, _ := url.Parse(clicore.DefaultControlPlaneURL)
			for _, read := range ciReads(fake) {
				if read.Host != def.Host {
					t.Errorf("read host = %s, want the default %s", read.Host, def.Host)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Errorf("the read left %v in the directory (%v)", entries, err)
			}

			fake.requests = nil
			runPipelineCIIn(ioctx, root, append(c.args, "--workspace", "acme", "--control-plane-url", reportTestBaseURL)...).wants(t, c.want)
			for _, read := range ciReads(fake) {
				if "https://"+read.Host != reportTestBaseURL {
					t.Errorf("read host = %s, want the one the flag names", read.Host)
				}
			}
		})
	}
}

func TestCIPipelineReadsWithoutALinkAskForTheWorkspace(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-outside-a-workspace", "pipeline-reads-without-a-link-ask-for-the-workspace")
	for _, args := range [][]string{
		{"list", "--commit", pipelineSHA, "--repo", "acme/app"}, {"providers"},
		{"view", "--pipeline", githubFixture.ref}, {"logs", "--pipeline", githubFixture.ref},
	} {
		fake := &ciFakeServer{}
		ioctx, root := unlinkedCIIO(t, fake)
		runPipelineCIIn(ioctx, root, args...).refuses(t, clicore.ExitUsage, "pass --workspace")
		if len(fake.requests) != 0 {
			t.Errorf("%v: refused after %+v", args, fake.requests)
		}
	}

	// A link that is present and wrong is an error, not a missing link.
	ioctx, root := unlinkedCIIO(t, &ciFakeServer{})
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := runPipelineCIIn(ioctx, root, "providers", "--workspace", "ws-acme")
	if run.err == nil || !strings.Contains(run.err.Error(), "parse") {
		t.Errorf("a broken manifest gives %v, want its parse error", run.err)
	}
}

// ciHostedRunVerbs are the verbs that read or act on the workspace's hosted
// runs, each with the workspace route its first call reaches.
var ciHostedRunVerbs = []struct {
	name  string
	args  []string
	route string
}{
	{"list", []string{"list"}, "GET /v1/workspaces/ws-acme/runs"},
	{"view", []string{"view", "run-9"}, "GET /v1/workspaces/ws-acme/runs/run-9"},
	{"logs", []string{"logs", "run-9"}, "GET /v1/workspaces/ws-acme/runs/run-9/logs"},
	{"start", []string{"start", "--repo", "acme/app", "--sha", pipelineSHA, "--reason", "why", "--yes"}, "POST /v1/workspaces/ws-acme/runs"},
	{"cancel", []string{"cancel", "run-9", "--reason", "why", "--yes"}, "POST /v1/workspaces/ws-acme/runs/run-9/cancel"},
	{"retry", []string{"retry", "run-9", "--reason", "why", "--yes"}, "POST /v1/workspaces/ws-acme/runs/run-9/retry"},
	{"status", []string{"status"}, "GET /v1/workspaces/ws-acme/ci/status"},
	{"usage", []string{"usage"}, "GET /v1/workspaces/ws-acme/ci/usage"},
	{"insights", []string{"insights", "run-9"}, "GET /v1/workspaces/ws-acme/runs/run-9"},
	{"timings", []string{"timings", "run-9"}, "GET /v1/workspaces/ws-acme/runs/run-9"},
	{"results", []string{"results", "run-9"}, "GET /v1/workspaces/ws-acme/runs/run-9"},
}

func TestCIHostedRunVerbsRunWithoutAWorkspace(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "reads-outside-a-workspace", "hosted-run-verbs-run-without-a-workspace")
	def, _ := url.Parse(clicore.DefaultControlPlaneURL)
	for _, c := range ciHostedRunVerbs {
		t.Run(c.name, func(t *testing.T) {
			fake := &ciFakeServer{responses: map[string]ciStubResponse{
				"GET /v1/workspaces": {status: http.StatusOK, body: map[string]any{"workspaces": []any{map[string]any{"id": "ws-acme", "slug": "acme"}}}},
			}}
			ioctx, root := unlinkedCIIO(t, fake)
			run := runPipelineCIIn(ioctx, root, append(c.args, "--workspace", "acme")...)
			if run.err != nil && strings.Contains(run.err.Error(), "cloud setup") {
				t.Fatalf("%s asks for putnami cloud setup: %v", c.name, run.err)
			}
			var reached bool
			for _, request := range fake.requests {
				if !strings.HasPrefix(request.Path, "/v1/workspaces/") {
					continue
				}
				if request.Host != def.Host {
					t.Errorf("%s %s host = %s, want the default %s", request.Method, request.Path, request.Host, def.Host)
				}
				if request.Method+" "+request.Path == c.route {
					reached = true
				}
			}
			if !reached {
				t.Errorf("%s never reached %s: %+v (err %v)", c.name, c.route, fake.requests, run.err)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Errorf("%s left %v in the directory (%v)", c.name, entries, err)
			}

			// Without --workspace, the verb names it before any call.
			fake = &ciFakeServer{}
			ioctx, root = unlinkedCIIO(t, fake)
			runPipelineCIIn(ioctx, root, c.args...).refuses(t, clicore.ExitUsage, "pass --workspace")
			if len(fake.requests) != 0 {
				t.Errorf("%s refused after %+v", c.name, fake.requests)
			}
		})
	}
}

func TestCIListOfHostedRunsRefusesPipelineFlags(t *testing.T) {
	spectest.Proves(t, pipelineFeature, "lists-the-pipelines-of-any-ci", "list-of-hosted-runs-refuses-pipeline-flags")
	for _, flag := range []string{"--pipeline=" + githubFixture.ref, "--job=lint", "--raw"} {
		fake := &ciFakeServer{}
		name, _, _ := strings.Cut(flag, "=")
		runPipelineCI(t, fake, "list", flag).refuses(t, clicore.ExitUsage, name+" does not apply with the list of hosted runs")
		if len(fake.requests) != 0 {
			t.Errorf("%s: a refused list called %+v", flag, fake.requests)
		}
	}
	// A switch set to false is no flag: list reads the hosted runs.
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: http.StatusOK, body: map[string]any{"runs": []any{}}},
	}}
	runPipelineCI(t, fake, "list", "--raw=false").wants(t)
}
