package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const (
	forkFeature = "tooling/observe-fork"
	relayReport = "https://putnami.app/w/acme/changes/acme%2Fshop%217"
	runPage     = "https://github.com/acme/shop/actions/runs/55"
	// hostile is text a fork writes to turn the note into Markdown, HTML, a
	// mention or a link, and the log into workflow commands.
	hostile = "@admin <img src=x onerror=alert(1)> [click](https://evil.example) **bold**"
	command = "::add-mask::leak\n::error title=pwned::owned"
)

// codeSpans matches a Markdown code span on one line.
var codeSpans = regexp.MustCompile("`[^`\n]*`")

var (
	forkHead  = strings.Repeat("c", 40)
	forkMerge = strings.Repeat("a", 40)
	forkBase  = strings.Repeat("b", 40)
	// otherMerge merges the fork's head too, but is not the pull request's
	// test merge commit: a merge of the same head into another branch.
	otherMerge = strings.Repeat("9", 40)
)

// forkDocument is the evidence document the fork's run kept: a pull request
// run of acme/shop with a failing test whose name and message are hostile.
func forkDocument(t *testing.T, edit func(*evidence.Document)) string {
	t.Helper()
	attempt := 1
	o := observe.Observation{
		Subject: ci.Subject{
			Provider: ci.ProviderGitHub, Host: "github.com", ProjectID: "1001", Repository: "acme/shop",
			SHA: forkMerge, BaseSHA: forkBase, Branch: "patch-1", ChangeRequest: &ci.ChangeRequest{Number: 7},
		},
		CI:           ci.Run{System: ci.SystemGitHubActions, RunID: "55", JobID: "test", Attempt: &attempt, URL: runPage},
		Producer:     observe.Producer{Name: "@putnami/observe", Version: "0.5.0"},
		ChangedFiles: []gitbase.ChangedFile{{Path: "README.md", Status: "modified"}},
		Diagnostics:  []diag.Diagnostic{diag.Warningf("observe.report_missing", "junit", "%s", command+" "+hostile)},
	}
	doc := observe.Evidence(o, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	doc.Tests = []evidence.ComponentTests{{Component: evidence.RootComponent, Passed: 3, Failed: 1, FailingCases: []evidence.FailingCase{
		{Name: hostile, Suite: command, Message: command + "\n" + hostile},
	}}}
	if edit != nil {
		edit(doc)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "observe-evidence.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// relayJob is the environment of a workflow_run job of acme/shop that the
// run 55 triggered, with the payload GitHub writes for a pull request from
// the fork mallory/shop: it lists no pull request. record receives what the
// fake reporter got.
func relayJob(t *testing.T, pullRequests string) (env map[string]string, record string) {
	return relayJobFrom(t, "2002", pullRequests)
}

// relayJobFrom is relayJob for a run of the repository headID.
func relayJobFrom(t *testing.T, headID, pullRequests string) (env map[string]string, record string) {
	t.Helper()
	payload := `{"action":"completed","workflow_run":{"id":55,"run_attempt":1,"html_url":"` + runPage + `",` +
		`"event":"pull_request","head_sha":"` + forkHead + `","head_branch":"patch-1",` +
		`"pull_requests":` + pullRequests + `,` +
		`"head_repository":{"id":` + headID + `,"full_name":"mallory/shop","owner":{"login":"mallory"}},` +
		`"repository":{"id":1001,"full_name":"acme/shop"}}}`
	event := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(event, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	record = t.TempDir()
	cli := filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + record + "/args'\n" +
		"cp \"$4\" '" + record + "/document'\n" +
		"echo '{\"protocolVersion\":2,\"command\":\"cloud evidence-report\",\"status\":\"success\",\"exitCode\":0,\"data\":{\"reportUrl\":\"" + relayReport + "\"}}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{
		"GITHUB_ACTIONS":         "true",
		"GITHUB_EVENT_NAME":      "workflow_run",
		"GITHUB_EVENT_PATH":      event,
		"GITHUB_REPOSITORY":      "acme/shop",
		"GITHUB_REPOSITORY_ID":   "1001",
		"GITHUB_SERVER_URL":      "https://github.com",
		"GITHUB_SHA":             strings.Repeat("d", 40),
		"GITHUB_REF":             "refs/heads/main",
		"GITHUB_RUN_ID":          "900",
		"GITHUB_TOKEN":           "ghs_basetoken",
		"PUTNAMI_CLI_EXECUTABLE": cli,
	}
	return env, record
}

// fakeGitHub answers the pull request lookup with pull request 7 at the
// fork's head, and records every request and the note.
type fakeGitHub struct {
	// pulls answers the pull request lookup; "" answers pull request 7.
	pulls string
	// mergeBase answers the compare; "" answers forkBase.
	mergeBase string
	requests  []string
	note      string
	tokens    []string
}

func (g *fakeGitHub) install(t *testing.T) {
	publishClient = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		line := req.Method + " " + req.URL.Path
		if req.URL.RawQuery != "" {
			line += "?" + req.URL.RawQuery
		}
		g.requests = append(g.requests, line)
		g.tokens = append(g.tokens, req.Header.Get("Authorization"))
		answer := `{"id":1,"html_url":"https://github.com/acme/shop/pull/7#issuecomment-1"}`
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			answer = `[{"number":6,"head":{"sha":"` + strings.Repeat("e", 40) + `"}},{"number":7,"head":{"sha":"` + forkHead + `"}}]`
			if g.pulls != "" {
				answer = g.pulls
			}
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/7"):
			answer = `{"number":7,"merge_commit_sha":"` + forkMerge + `"}`
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/commits/"+forkMerge):
			answer = `{"sha":"` + forkMerge + `","parents":[{"sha":"` + forkBase + `"},{"sha":"` + forkHead + `"}]}`
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/commits/"+otherMerge):
			answer = `{"sha":"` + otherMerge + `","parents":[{"sha":"` + forkBase + `"},{"sha":"` + forkHead + `"}]}`
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/commits/"):
			answer = `{"parents":[{"sha":"` + forkBase + `"}]}`
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compare/"):
			base := forkBase
			if g.mergeBase != "" {
				base = g.mergeBase
			}
			answer = `{"merge_base_commit":{"sha":"` + base + `"}}`
		case req.Method == http.MethodGet:
			answer = `[]`
		case strings.HasSuffix(req.URL.Path, "/comments"):
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			g.note = body["body"]
		}
		return &http.Response{StatusCode: 201, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(answer)), Request: req}, nil
	})}
	t.Cleanup(func() { publishClient = nil })
}

func relayRun(t *testing.T, env map[string]string, args ...string) (stdout, stderr string) {
	t.Helper()
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(append([]string{"run"}, args...), func(k string) string { return env[k] }, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	return out.String(), errOut.String()
}

func relayedDocument(t *testing.T, record, log string) *evidence.Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(record, "document"))
	if err != nil {
		t.Fatalf("the reporter got no document: %v\n%s", err, log)
	}
	doc, problems := evidence.ParseAndValidate(data)
	if diag.HasErrors(problems) {
		t.Fatalf("the relayed document does not validate: %v", problems)
	}
	return doc
}

// assertInertLog fails when the log carries a text of the document, or a
// line GitHub Actions would read as a workflow command.
func assertInertLog(t *testing.T, log string) {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "::") {
			t.Fatalf("the log holds a workflow command line %q:\n%s", line, log)
		}
	}
	for _, text := range []string{"@admin", "<img", "evil.example", "add-mask", "pwned", "mallory", "patch-1"} {
		if strings.Contains(log, text) {
			t.Fatalf("the log quotes %q of the document:\n%s", text, log)
		}
	}
}

func TestAForkWorkflowRunTargetsTheBasePullRequest(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-runs-in-a-workflow-run-job", "a-fork-workflow-run-targets-the-base-pull-request")
	spectest.Proves(t, forkFeature, "relay-finds-the-pull-request", "a-fork-workflow-run-targets-the-base-pull-request")
	spectest.Proves(t, forkFeature, "relay-reads-a-kept-document", "a-kept-document-is-relayed-without-observing")
	spectest.Proves(t, forkFeature, "relay-keeps-the-text-inert", "the-note-quotes-every-document-text")
	spectest.Proves(t, forkFeature, "relay-keeps-the-text-inert", "the-log-prints-no-document-text")
	env, record := relayJob(t, `[]`)
	var gh fakeGitHub
	gh.install(t)
	// The job runs in an empty directory: the relay runs no Git command and
	// reads no report.
	stdout, stderr := relayRun(t, env, "--from-evidence", forkDocument(t, nil), "--upload", "--publish")

	doc := relayedDocument(t, record, stdout)
	if doc.Subject.Repository != "acme/shop" || doc.Subject.ProjectID != "1001" || doc.Subject.ChangeRequest == nil || doc.Subject.ChangeRequest.Number != 7 {
		t.Fatalf("relayed subject %+v", doc.Subject)
	}
	if doc.Subject.Branch != "" {
		t.Fatalf("the fork's branch %q was relayed", doc.Subject.Branch)
	}
	if doc.Subject.SHA != forkMerge || doc.Subject.BaseSHA != forkBase || doc.CI.RunID != "55" || doc.CI.URL != runPage || doc.CI.JobID != "test" {
		t.Fatalf("relayed subject %+v, CI %+v", doc.Subject, doc.CI)
	}
	if len(doc.Tests) != 1 || doc.Tests[0].Failed != 1 {
		t.Fatalf("the relay dropped the fork's tests: %+v", doc.Tests)
	}
	want := []string{
		"GET /repos/acme/shop/pulls?head=mallory%3Apatch-1&page=1&per_page=100&state=open",
		"GET /repos/acme/shop/pulls/7",
		"GET /repos/acme/shop/commits/" + forkMerge,
		"POST /repos/acme/shop/statuses/" + forkHead,
		"GET /repos/acme/shop/issues/7/comments?page=1&per_page=100",
		"POST /repos/acme/shop/issues/7/comments",
	}
	if !slices.Equal(gh.requests, want) {
		t.Fatalf("requests\n%s\nwant\n%s", strings.Join(gh.requests, "\n"), strings.Join(want, "\n"))
	}
	for _, token := range gh.tokens {
		if token != "Bearer ghs_basetoken" {
			t.Fatalf("a request carried %q", token)
		}
	}
	if !strings.Contains(gh.note, "[Open the full report]("+relayReport+")") || !strings.Contains(gh.note, "[CI run]("+runPage+")") {
		t.Fatalf("note %s", gh.note)
	}
	// Every text of the document is a code span on one line.
	if !strings.Contains(gh.note, "- `::add-mask::leak ::error title=pwned::owned > "+hostile+"`") {
		t.Fatalf("note %s", gh.note)
	}
	outside := codeSpans.ReplaceAllString(gh.note, "")
	for _, text := range []string{"@admin", "<img", "evil.example", "**bold", "add-mask", "\n::"} {
		if strings.Contains(outside, text) {
			t.Fatalf("the note carries %q outside a code span:\n%s", text, gh.note)
		}
	}

	if !strings.Contains(stdout, "relayed the evidence of GitHub Actions run 55 to github.com/acme/shop (project 1001)") ||
		!strings.Contains(stdout, "pull request #7, head commit "+forkHead[:12]) || !strings.Contains(stdout, "Full report: "+relayReport) {
		t.Fatalf("summary\n%s", stdout)
	}
	assertInertLog(t, stdout)
	if strings.Contains(stderr, "@admin") || strings.Contains(stderr, "add-mask") {
		t.Fatalf("stderr quotes the document: %s", stderr)
	}
}

func TestASameRepositoryWorkflowRunTakesThePayloadPullRequest(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-finds-the-pull-request", "a-same-repository-workflow-run-takes-the-payload-pull-request")
	env, record := relayJobFrom(t, "1001", `[{"number":7,"head":{"sha":"`+forkHead+`"},"base":{"repo":{"id":1001}}}]`)
	var gh fakeGitHub
	gh.install(t)
	stdout, _ := relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.CI.JobID = "test\n::error::x" }), "--upload", "--publish")
	doc := relayedDocument(t, record, stdout)
	if doc.Subject.ChangeRequest == nil || doc.Subject.ChangeRequest.Number != 7 {
		t.Fatalf("relayed subject %+v", doc.Subject)
	}
	if doc.CI.JobID != "" {
		t.Fatalf("a job id that is no job key was relayed: %q", doc.CI.JobID)
	}
	if len(gh.requests) != 5 || strings.Contains(gh.requests[0], "/pulls?") {
		t.Fatalf("the payload's pull request needs no lookup, requests %v", gh.requests)
	}
}

func TestAnUnresolvedPullRequestRelaysNothingFromAFork(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-finds-the-pull-request", "an-unresolved-pull-request-relays-nothing-from-a-fork")
	// A fork whose pull request moved on, or closed, relays nothing.
	env, record := relayJob(t, `[]`)
	gh := fakeGitHub{pulls: `[]`}
	gh.install(t)
	stdout, _ := relayRun(t, env, "--from-evidence", forkDocument(t, nil), "--upload", "--publish")
	if !strings.Contains(stdout, "warning observe.evidence_refused") {
		t.Fatalf("summary\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatal("a fork's document without its pull request reached the reporter")
	}

	// A run of the job's repository uploads without a pull request and
	// publishes nothing.
	env, record = relayJobFrom(t, "1001", `[]`)
	gh = fakeGitHub{pulls: `[]`}
	gh.install(t)
	stdout, _ = relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.Subject.SHA = forkHead }), "--upload", "--publish")
	if doc := relayedDocument(t, record, stdout); doc.Subject.ChangeRequest != nil {
		t.Fatalf("an unresolved run keeps the document's pull request %+v", doc.Subject.ChangeRequest)
	}
	for _, r := range gh.requests {
		if strings.HasPrefix(r, "POST") {
			t.Fatalf("published: %v", gh.requests)
		}
	}
	for _, code := range []string{"info observe.pull_request_unresolved", "info observe.publish_skipped"} {
		if !strings.Contains(stdout, code) {
			t.Fatalf("summary lacks %s:\n%s", code, stdout)
		}
	}
}

func TestADocumentNamingAnotherCommitIsRefused(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-refuses-another-repository", "a-document-naming-another-commit-is-refused")
	env, record := relayJob(t, `[]`)
	var gh fakeGitHub
	gh.install(t)
	stdout, _ := relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.Subject.SHA = strings.Repeat("1", 40) }), "--upload", "--publish")
	if !strings.Contains(stdout, "warning observe.evidence_refused") {
		t.Fatalf("summary\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatal("a document naming another commit reached the reporter")
	}

	// Another merge commit of the same head is not the pull request's.
	env, record = relayJob(t, `[]`)
	gh = fakeGitHub{}
	gh.install(t)
	stdout, _ = relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.Subject.SHA = otherMerge }), "--upload", "--publish")
	if !strings.Contains(stdout, "warning observe.evidence_refused") || !strings.Contains(stdout, "nor the merge commit of pull request #7") {
		t.Fatalf("summary\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatal("another merge commit of the head reached the reporter")
	}

	// The head commit itself is the run's, but a base that is not its merge
	// base is refused.
	env, record = relayJob(t, `[]`)
	gh = fakeGitHub{mergeBase: strings.Repeat("f", 40)}
	gh.install(t)
	stdout, _ = relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.Subject.SHA = forkHead }), "--upload")
	if !strings.Contains(stdout, "warning observe.evidence_refused") {
		t.Fatalf("summary\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatal("a document naming another base reached the reporter")
	}
	// The run's head commit with its merge base is relayed.
	env, record = relayJob(t, `[]`)
	gh = fakeGitHub{}
	gh.install(t)
	stdout, _ = relayRun(t, env, "--from-evidence", forkDocument(t, func(d *evidence.Document) { d.Subject.SHA = forkHead }), "--upload")
	if doc := relayedDocument(t, record, stdout); doc.Subject.BaseSHA != forkBase {
		t.Fatalf("relayed subject %+v", doc.Subject)
	}
}

func TestADocumentNamingAnotherRepositoryIsRefused(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-refuses-another-repository", "a-document-naming-another-repository-is-refused")
	spectest.Proves(t, forkFeature, "relay-refuses-another-repository", "a-document-naming-another-pull-request-is-refused")
	for name, edit := range map[string]func(*evidence.Document){
		"another path":         func(d *evidence.Document) { d.Subject.Repository = "acme/bank" },
		"another id":           func(d *evidence.Document) { d.Subject.ProjectID = "2002" },
		"another host":         func(d *evidence.Document) { d.Subject.Host = "github.example.com" },
		"another pull request": func(d *evidence.Document) { d.Subject.ChangeRequest.Number = 8 },
		"another CI": func(d *evidence.Document) {
			d.CI.System = evidence.CISystemGitLabCI
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, record := relayJob(t, `[]`)
			var gh fakeGitHub
			gh.install(t)
			stdout, _ := relayRun(t, env, "--from-evidence", forkDocument(t, edit), "--upload", "--publish")
			if !strings.Contains(stdout, "warning observe.evidence_refused") {
				t.Fatalf("summary\n%s", stdout)
			}
			if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
				t.Fatal("a refused document reached the reporter")
			}
			for _, r := range gh.requests {
				if r[:4] == "POST" {
					t.Fatalf("a refused document was published: %v", gh.requests)
				}
			}
			assertInertLog(t, stdout)
		})
	}
}

func TestOutsideAWorkflowRunJobNothingIsRelayed(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-runs-in-a-workflow-run-job", "outside-a-workflow-run-job-nothing-is-relayed")
	for name, edit := range map[string]func(map[string]string){
		"a pull request job": func(env map[string]string) { env["GITHUB_EVENT_NAME"] = "pull_request" },
		"a laptop":           func(env map[string]string) { delete(env, "GITHUB_ACTIONS") },
		"no payload":         func(env map[string]string) { env["GITHUB_EVENT_PATH"] = filepath.Join(t.TempDir(), "missing.json") },
		"no repository id":   func(env map[string]string) { delete(env, "GITHUB_REPOSITORY_ID") },
		"no token":           func(env map[string]string) { delete(env, "GITHUB_TOKEN") },
		"a push run": func(env map[string]string) {
			data, _ := os.ReadFile(env["GITHUB_EVENT_PATH"])
			path := filepath.Join(t.TempDir(), "push.json")
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), `"event":"pull_request"`, `"event":"push"`, 1)), 0o644)
			env["GITHUB_EVENT_PATH"] = path
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, record := relayJob(t, `[]`)
			edit(env)
			var gh fakeGitHub
			gh.install(t)
			stdout, _ := relayRun(t, env, "--from-evidence", forkDocument(t, nil), "--upload", "--publish")
			if !strings.Contains(stdout, "warning observe.evidence_refused") || len(gh.requests) != 0 {
				t.Fatalf("requests %v, summary\n%s", gh.requests, stdout)
			}
			if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
				t.Fatal("the document reached the reporter")
			}
		})
	}
}

func TestAnInvalidOrOversizedDocumentIsRefused(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-reads-a-kept-document", "an-invalid-or-oversized-document-is-refused")
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"protocolVersion":1,"subject":{"provider":"github","repository":"`+command+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "oversized.json")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64<<20 + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for name, path := range map[string]string{"invalid": invalid, "oversized": oversized, "missing": filepath.Join(dir, "missing.json")} {
		t.Run(name, func(t *testing.T) {
			env, record := relayJob(t, `[]`)
			var gh fakeGitHub
			gh.install(t)
			stdout, _ := relayRun(t, env, "--from-evidence", path, "--upload", "--publish")
			if !strings.Contains(stdout, "warning observe.evidence_refused") || len(gh.requests) != 0 {
				t.Fatalf("requests %v, summary\n%s", gh.requests, stdout)
			}
			if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
				t.Fatal("the document reached the reporter")
			}
			assertInertLog(t, stdout)
		})
	}
}

func TestAnObservationFlagWithFromEvidenceIsAUsageError(t *testing.T) {
	spectest.Proves(t, forkFeature, "relay-reads-a-kept-document", "an-observation-flag-with-from-evidence-is-a-usage-error")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"--from-evidence"},
		{"--from-evidence", "e.json", "--base", "main"},
		{"--from-evidence", "e.json", "--junit", "r.xml"},
		{"--from-evidence", "e.json", "--evidence", "out.json"},
		{"--from-evidence", "e.json", "--open"},
		{"--from-evidence", "e.json", "--no-fetch"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"run"}, args...), func(string) string { return "" }, &stdout, &stderr); code != 2 {
			t.Fatalf("%v exits %d, want 2: %s", args, code, stderr.String())
		}
	}
}
