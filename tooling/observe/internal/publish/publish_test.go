package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const (
	feature     = "tooling/observe-host-report"
	githubToken = "ghs_fixturetoken"
	gitlabToken = "glpat-fixturetoken"
)

var (
	mergeSHA = strings.Repeat("a", 40)
	baseSHA  = strings.Repeat("b", 40)
	headSHA  = strings.Repeat("c", 40)
)

// exchange is one recorded HTTP request and its answer.
type exchange struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		// Body is the exact JSON body, compared as JSON.
		Body json.RawMessage `json:"body"`
		// NoteGolden names a testdata file that holds the note the body
		// carries.
		NoteGolden string `json:"noteGolden"`
		// NoteFromObservation says the body carries Note of the test's
		// observation.
		NoteFromObservation bool `json:"noteFromObservation"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
}

// replay answers the requests of one fixture in order and fails the test on
// any other request.
type replay struct {
	t         *testing.T
	exchanges []exchange
	note      string
	mu        sync.Mutex
	next      int
	tokens    []string
}

func load(t *testing.T, name, note string) *replay {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Exchanges []exchange `json:"exchanges"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &replay{t: t, exchanges: fixture.Exchanges, note: note}
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.t
	if r.next == len(r.exchanges) {
		t.Errorf("unexpected request %s %s", req.Method, req.URL)
		return nil, io.ErrUnexpectedEOF
	}
	e := r.exchanges[r.next]
	r.next++
	if req.Method != e.Request.Method || req.URL.String() != e.Request.URL {
		t.Errorf("request %d: got %s %s, want %s %s", r.next, req.Method, req.URL, e.Request.Method, e.Request.URL)
	}
	for name, want := range e.Request.Headers {
		if got := req.Header.Get(name); got != want {
			t.Errorf("request %d: header %s = %q, want %q", r.next, name, got, want)
		}
	}
	r.tokens = append(r.tokens, req.Header.Get("Authorization")+req.Header.Get("PRIVATE-TOKEN"))
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if len(e.Request.Body) > 0 {
		var got, want any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request %d: body %q: %v", r.next, body, err)
		}
		if err := json.Unmarshal(e.Request.Body, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("request %d: body\n%s\nwant\n%s", r.next, body, e.Request.Body)
		}
	}
	if e.Request.NoteGolden != "" || e.Request.NoteFromObservation {
		want := r.note
		if e.Request.NoteGolden != "" {
			golden, err := os.ReadFile(filepath.Join("testdata", e.Request.NoteGolden))
			if err != nil {
				t.Fatal(err)
			}
			want = string(golden)
		}
		var note map[string]string
		if err := json.Unmarshal(body, &note); err != nil || len(note) != 1 || note["body"] != want {
			t.Errorf("request %d: note body\n%s\nwant\n%s", r.next, note["body"], want)
		}
	}
	header := http.Header{"Content-Type": {"application/json"}}
	for name, value := range e.Response.Headers {
		header.Set(name, value)
	}
	return &http.Response{
		StatusCode: e.Response.Status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(e.Response.Body)),
		Request:    req,
	}, nil
}

// done fails the test when a recorded request was not sent.
func (r *replay) done() {
	r.t.Helper()
	if r.next != len(r.exchanges) {
		r.t.Errorf("%d of %d recorded requests were sent", r.next, len(r.exchanges))
	}
}

func analysis() analyze.Result {
	return analyze.Result{
		Components: []evidence.Component{
			{ID: "apps/web", Name: "web", Ecosystem: "node", Source: "npm-workspaces", Impact: evidence.ImpactDependent, DependsOn: []string{"packages/pricing"}},
			{ID: "docs", Ecosystem: "node", Source: "npm-workspaces", Impact: evidence.ImpactUnaffected},
			{ID: "packages/pricing", Name: "@acme/pricing", Ecosystem: "node", Source: "npm-workspaces", Impact: evidence.ImpactChanged},
		},
		Tests: []evidence.ComponentTests{{
			Component: "packages/pricing", Passed: 4, Failed: 1,
			FailingCases: []evidence.FailingCase{{Suite: "Calculator", Name: "applies the discount", Message: "expected 90, got 100\n    at discount.test.ts:12"}},
		}},
		Coverage: []evidence.ComponentCoverage{{Component: "packages/pricing", CoveredLines: 40, TotalLines: 50}},
		ChangedLineCoverage: &evidence.ChangedLineCoverage{CoveredLines: 8, TotalLines: 10, Files: []evidence.ChangedLineCoverageFile{
			{Path: "packages/pricing/src/line|item.ts", CoveredLines: 2, TotalLines: 2},
			{Path: "packages/pricing/src/discount.ts", CoveredLines: 6, TotalLines: 8, UncoveredLines: []int{12, 13}},
		}},
		DependencyChanges: []evidence.DependencyChange{{Ecosystem: "npm", Name: "lodash", From: "4.17.20", To: "4.17.21", Change: evidence.DependencyUpgraded, Lockfile: "package-lock.json"}},
	}
}

func pullRequest() observe.Observation {
	return observe.Observation{
		Subject: ci.Subject{
			Provider: ci.ProviderGitHub, Host: "github.com", ProjectID: "123456", Repository: "acme/shop",
			SHA: mergeSHA, BaseSHA: baseSHA, ChangeRequest: &ci.ChangeRequest{Number: 42, HeadSHA: headSHA},
		},
		CI:          ci.Run{System: ci.SystemGitHubActions, RunID: "987", URL: "https://github.com/acme/shop/actions/runs/987"},
		Analysis:    analysis(),
		Diagnostics: []diag.Diagnostic{diag.Warningf("observe.report_missing", "junit", "no JUnit report was found")},
	}
}

func mergeRequest() observe.Observation {
	return observe.Observation{
		Subject: ci.Subject{
			Provider: ci.ProviderGitLab, Host: "gitlab.com", ProjectID: "4242", Repository: "acme/shop",
			SHA: headSHA, BaseSHA: baseSHA, ChangeRequest: &ci.ChangeRequest{Number: 7, HeadSHA: headSHA},
		},
		CI:       ci.Run{System: ci.SystemGitLabCI, RunID: "555", JobID: "666", URL: "https://gitlab.com/acme/shop/-/pipelines/555"},
		Analysis: analysis(),
	}
}

func envOf(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

var githubEnv = map[string]string{"GITHUB_TOKEN": githubToken}

var gitlabEnv = map[string]string{
	"PUTNAMI_GITLAB_TOKEN": gitlabToken,
	"CI_API_V4_URL":        "https://gitlab.com/api/v4",
	"CI_SERVER_URL":        "https://gitlab.com",
	"CI_PROJECT_URL":       "https://gitlab.com/acme/shop",
	"CI_COMMIT_SHA":        headSHA,
}

// publish runs Run against a fixture and returns its diagnostics and output.
func publish(t *testing.T, fixture string, o observe.Observation, env map[string]string) ([]diag.Diagnostic, string, *replay) {
	t.Helper()
	r := load(t, fixture, Note(o))
	var out bytes.Buffer
	diags := Run(context.Background(), Request{Observation: o, Getenv: envOf(env), Client: &http.Client{Transport: r}, Output: &out})
	r.done()
	noLeak(t, diags, out.String(), githubToken, gitlabToken, "secret")
	return diags, out.String(), r
}

// noLeak fails when a token appears in a diagnostic or in the output.
func noLeak(t *testing.T, diags []diag.Diagnostic, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		for _, d := range diags {
			if strings.Contains(d.Message, secret) || strings.Contains(d.Field, secret) {
				t.Errorf("diagnostic %+v quotes %q", d, secret)
			}
		}
		if strings.Contains(output, secret) {
			t.Errorf("output %q quotes %q", output, secret)
		}
	}
}

func TestAGitHubPullRequestGetsAStatusAndANote(t *testing.T) {
	spectest.Proves(t, feature, "publish-status", "a-github-pull-request-gets-a-status-and-a-note")
	spectest.Proves(t, feature, "publish-note", "a-github-pull-request-gets-a-status-and-a-note")
	diags, out, _ := publish(t, "github-first-run.json", pullRequest(), githubEnv)
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	want := "putnami observe: set the commit status putnami/observe on cccccccccccc\n" +
		"putnami observe: created the summary note https://github.com/acme/shop/pull/42#issuecomment-2468024680\n"
	if out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

func TestAGitLabMergeRequestGetsAStatusAndANote(t *testing.T) {
	spectest.Proves(t, feature, "publish-status", "a-gitlab-merge-request-gets-a-status-and-a-note")
	spectest.Proves(t, feature, "publish-note", "a-gitlab-merge-request-gets-a-status-and-a-note")
	diags, out, _ := publish(t, "gitlab-first-run.json", mergeRequest(), gitlabEnv)
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	if !strings.HasSuffix(out, "created the summary note https://gitlab.com/acme/shop/-/merge_requests/7#note_2210003\n") {
		t.Fatalf("output %q", out)
	}
}

func TestASecondRunUpdatesTheSameNote(t *testing.T) {
	spectest.Proves(t, feature, "publish-note", "a-second-run-updates-the-same-note")
	t.Run("github", func(t *testing.T) {
		diags, out, _ := publish(t, "github-second-run.json", pullRequest(), githubEnv)
		if len(diags) != 0 || !strings.Contains(out, "updated the summary note https://github.com/acme/shop/pull/42#issuecomment-2468024680") {
			t.Fatalf("diagnostics %v, output %q", diags, out)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		perPage = 2
		t.Cleanup(func() { perPage = 100 })
		diags, out, _ := publish(t, "gitlab-second-run.json", mergeRequest(), gitlabEnv)
		if len(diags) != 0 || !strings.Contains(out, "updated the summary note https://gitlab.com/acme/shop/-/merge_requests/7#note_2210003") {
			t.Fatalf("diagnostics %v, output %q", diags, out)
		}
	})
}

func TestASelfManagedGitLabUsesItsOwnAPIURL(t *testing.T) {
	spectest.Proves(t, feature, "publish-host-endpoints", "a-self-managed-gitlab-uses-its-own-api-url")
	o := mergeRequest()
	o.Subject.Host = "git.example.org"
	o.CI.URL = ""
	diags, _, _ := publish(t, "gitlab-self-managed.json", o, map[string]string{
		"PUTNAMI_GITLAB_TOKEN": gitlabToken,
		"CI_SERVER_URL":        "https://git.example.org/gitlab/",
		"CI_COMMIT_SHA":        headSHA,
	})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}

	t.Run("no API URL", func(t *testing.T) {
		diags, _, _ := publish(t, "none.json", mergeRequest(), map[string]string{"PUTNAMI_GITLAB_TOKEN": gitlabToken})
		if len(diags) != 1 || diags[0].Code != CodeFailed || !strings.Contains(diags[0].Message, "neither CI_API_V4_URL nor CI_SERVER_URL") {
			t.Fatalf("diagnostics %v", diags)
		}
	})
}

func TestAMergedResultsPipelineGetsTheStatusOnItsOwnCommit(t *testing.T) {
	spectest.Proves(t, feature, "publish-status", "a-gitlab-merge-request-gets-a-status-and-a-note")
	o := mergeRequest()
	o.Subject.SHA = mergeSHA
	env := map[string]string{}
	for k, v := range gitlabEnv {
		env[k] = v
	}
	env["CI_COMMIT_SHA"] = strings.ToUpper(mergeSHA)
	diags, out, _ := publish(t, "gitlab-merged-results.json", o, env)
	if len(diags) != 0 || !strings.Contains(out, "on aaaaaaaaaaaa") {
		t.Fatalf("diagnostics %v, output %q", diags, out)
	}

	t.Run("no commit", func(t *testing.T) {
		o := mergeRequest()
		o.Subject.ChangeRequest.HeadSHA = ""
		env["CI_COMMIT_SHA"] = "main"
		r := load(t, "gitlab-first-run.json", Note(o))
		r.exchanges = r.exchanges[1:]
		diags := Run(context.Background(), Request{Observation: o, Getenv: envOf(env), Client: &http.Client{Transport: r}})
		r.done()
		if len(diags) != 1 || !strings.Contains(diags[0].Message, "names no commit of merge request !7") {
			t.Fatalf("diagnostics %v", diags)
		}
	})
}

func TestTheTokenGoesOnlyToAnHTTPSAPI(t *testing.T) {
	spectest.Proves(t, feature, "publish-host-endpoints", "the-token-goes-only-to-an-https-api")
	for name, apiURL := range map[string]string{
		"plain http":  "http://gitlab.example.com/api/v4",
		"credentials": "https://ci:secret@gitlab.example.com/api/v4",
		"query":       "https://gitlab.example.com/api/v4?token=secret",
		"not a URL":   "https://gitlab.example.com:port/api/v4",
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"PUTNAMI_GITLAB_TOKEN": gitlabToken, "CI_API_V4_URL": apiURL}
			diags, _, _ := publish(t, "none.json", mergeRequest(), env)
			if len(diags) != 1 || diags[0].Code != CodeFailed || diags[0].Severity != diag.Warning {
				t.Fatalf("diagnostics %v", diags)
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		diags, _, r := publish(t, "gitlab-redirect.json", mergeRequest(), gitlabEnv)
		if len(diags) != 2 || !strings.Contains(diags[0].Message, "302") || !strings.Contains(diags[1].Message, "301") {
			t.Fatalf("diagnostics %v", diags)
		}
		if len(r.tokens) != 2 {
			t.Fatalf("the redirects were followed: %d requests", len(r.tokens))
		}
	})
	t.Run("GitHub Enterprise", func(t *testing.T) {
		env := map[string]string{"GITHUB_TOKEN": githubToken, "GITHUB_API_URL": "http://ghe.example.com/api/v3"}
		diags, _, _ := publish(t, "none.json", pullRequest(), env)
		if len(diags) != 1 || diags[0].Code != CodeFailed {
			t.Fatalf("diagnostics %v", diags)
		}
	})
}

func TestAMissingTokenPublishesNothing(t *testing.T) {
	spectest.Proves(t, feature, "publish-fails-open", "a-missing-token-publishes-nothing")
	for name, o := range map[string]observe.Observation{"github": pullRequest(), "gitlab": mergeRequest()} {
		t.Run(name, func(t *testing.T) {
			diags, _, _ := publish(t, "none.json", o, map[string]string{"GITHUB_TOKEN": " ", "CI_API_V4_URL": "https://gitlab.com/api/v4"})
			if len(diags) != 1 || diags[0].Code != CodeTokenMissing || diags[0].Severity != diag.Warning {
				t.Fatalf("diagnostics %v", diags)
			}
		})
	}
}

func TestOutsideAChangeRequestNothingIsPublished(t *testing.T) {
	spectest.Proves(t, feature, "publish-fails-open", "outside-a-change-request-nothing-is-published")
	push := pullRequest()
	push.Subject.ChangeRequest = nil
	generic := pullRequest()
	generic.Subject.Provider = ci.ProviderGeneric
	for name, o := range map[string]observe.Observation{"push": push, "generic": generic} {
		t.Run(name, func(t *testing.T) {
			diags, _, _ := publish(t, "none.json", o, githubEnv)
			if len(diags) != 1 || diags[0].Code != CodeSkipped || diags[0].Severity != diag.Info {
				t.Fatalf("diagnostics %v", diags)
			}
		})
	}
}

func TestARefusedCallIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "publish-fails-open", "a-refused-call-is-a-diagnostic")
	diags, out, _ := publish(t, "github-refused.json", pullRequest(), githubEnv)
	if len(diags) != 2 || out != "" {
		t.Fatalf("diagnostics %v, output %q", diags, out)
	}
	for _, d := range diags {
		if d.Code != CodeFailed || d.Severity != diag.Warning || !strings.Contains(d.Message, "403: Resource not accessible by integration") {
			t.Fatalf("diagnostic %+v", d)
		}
	}

	t.Run("no head commit", func(t *testing.T) {
		o := pullRequest()
		o.Subject.ChangeRequest.HeadSHA = ""
		r := load(t, "github-first-run.json", Note(o))
		r.exchanges = r.exchanges[1:]
		var out bytes.Buffer
		diags := Run(context.Background(), Request{Observation: o, Getenv: envOf(githubEnv), Client: &http.Client{Transport: r}, Output: &out})
		r.done()
		if len(diags) != 1 || diags[0].Code != CodeFailed || !strings.Contains(out.String(), "created the summary note") {
			t.Fatalf("diagnostics %v, output %q", diags, out.String())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		blocked := roundTrip(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		diags := Run(context.Background(), Request{Observation: pullRequest(), Getenv: envOf(githubEnv), Client: &http.Client{Transport: blocked}, Timeout: 1})
		if len(diags) != 2 || !strings.Contains(diags[1].Message, "ran past") {
			t.Fatalf("diagnostics %v", diags)
		}
	})
	t.Run("unreadable answer", func(t *testing.T) {
		garbled := roundTrip(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<html>")), Request: req}, nil
		})
		diags := Run(context.Background(), Request{Observation: mergeRequest(), Getenv: envOf(gitlabEnv), Client: &http.Client{Transport: garbled}})
		if len(diags) != 1 || !strings.Contains(diags[0].Message, "not the expected JSON") {
			t.Fatalf("diagnostics %v", diags)
		}
	})
	t.Run("malformed repository", func(t *testing.T) {
		o := pullRequest()
		o.Subject.Repository = "shop"
		diags, _, _ := publish(t, "none.json", o, githubEnv)
		if len(diags) != 1 || diags[0].Code != CodeFailed {
			t.Fatalf("diagnostics %v", diags)
		}
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTheNoteRendersTheAnalysis(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "note-github.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Note(pullRequest()); got != string(golden) {
		t.Fatalf("note\n%s\nwant\n%s", got, golden)
	}
	empty := Note(observe.Observation{})
	if !strings.HasPrefix(empty, Marker+"\n") || !strings.Contains(empty, "No component holds a changed file.") {
		t.Fatalf("empty note %q", empty)
	}
}

func TestTheNoteQuotesRepositoryText(t *testing.T) {
	if got := code("a`b\n<img src=x>"); got != "`a'b <img src=x>`" {
		t.Fatalf("code %q", got)
	}
	if got := cell(code("x|y")); got != "`x\\|y`" {
		t.Fatalf("cell %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := code(long); !strings.HasSuffix(got, "...`") || len([]rune(got)) != maxCell+5 {
		t.Fatalf("long code %q", got)
	}
}

func TestTheNoteAndTheDescriptionAreBounded(t *testing.T) {
	a := analysis()
	a.Components = nil
	for i := range 200 {
		a.Components = append(a.Components, evidence.Component{ID: strings.Repeat("x", 150) + string(rune('a'+i%26)), Impact: evidence.ImpactChanged})
	}
	for i := range 40 {
		a.DependencyChanges = append(a.DependencyChanges, evidence.DependencyChange{Name: "dep", From: "1", To: "2", Change: evidence.DependencyAdded, Lockfile: "go.sum"})
		a.Tests[0].FailingCases = append(a.Tests[0].FailingCases, evidence.FailingCase{Name: "case " + string(rune('a'+i%26))})
		a.ChangedLineCoverage.Files = append(a.ChangedLineCoverage.Files, evidence.ChangedLineCoverageFile{Path: "f.go", TotalLines: 1})
	}
	o := pullRequest()
	o.Analysis = a
	note := Note(o)
	for _, want := range []string{"... and 170 more components.", "- ... and 21 more", "... and 12 more files.", "... and 11 more dependency changes."} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q", want)
		}
	}
	if got := bounded(strings.Repeat("line\n", maxNoteBytes)); len(got) > maxNoteBytes+100 || !strings.HasSuffix(got, "the job log holds the rest.\n") {
		t.Fatalf("bounded note of %d bytes ends %q", len(got), got[len(got)-40:])
	}
	measured := observe.Observation{Subject: ci.Subject{BaseSHA: baseSHA}}
	measured.Analysis = analyze.Result{Components: []evidence.Component{{Impact: evidence.ImpactDependent}}, DependencyChanges: make([]evidence.DependencyChange, 2)}
	if d := Description(measured); d != "0 changed, 1 dependent components; changed lines not measured; 2 dependency changes" {
		t.Fatalf("description %q", d)
	}
	measured.Subject.BaseSHA = ""
	if d := Description(measured); !strings.HasPrefix(d, "the base of the change was not resolved") || len(d) > maxDescription {
		t.Fatalf("description without a base %q", d)
	}
	huge := analyze.Result{ChangedLineCoverage: &evidence.ChangedLineCoverage{CoveredLines: 99999, TotalLines: 100000}}
	for range 100000 {
		huge.Components = append(huge.Components, evidence.Component{Impact: evidence.ImpactChanged})
	}
	huge.DependencyChanges = make([]evidence.DependencyChange, 100000)
	if d := Description(observe.Observation{Subject: ci.Subject{BaseSHA: baseSHA}, Analysis: huge}); len([]rune(d)) > maxDescription {
		t.Fatalf("description of %d characters: %q", len([]rune(d)), d)
	}
}

func TestHostMessages(t *testing.T) {
	for body, want := range map[string]string{
		`{"message":"Not Found"}`:                         "Not Found",
		`{"message":{"base":["Commit status exists"]}}`:   `{"base":["Commit status exists"]}`,
		`{"error":"insufficient_scope"}`:                  "insufficient_scope",
		`<html>`:                                          "",
		`{"message":"` + strings.Repeat("m", 400) + `"}`:  strings.Repeat("m", maxMessageBytes) + "...",
		`{"documentation_url":"https://docs.github.com"}`: "",
	} {
		if got := hostMessage([]byte(body)); got != want {
			t.Errorf("hostMessage(%s) = %q, want %q", body, got, want)
		}
	}
}
