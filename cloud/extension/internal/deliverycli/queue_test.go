package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

const testGitHubHost = "github.test"

// githubFake serves the GitHub REST calls of `ci queue` over a set of
// rulesets it keeps, so a write shows in the next read.
type githubFake struct {
	t        *testing.T
	admin    bool
	rulesets map[int64]*githubRuleset
	nextID   int64
	// refuse answers a method and path with a status instead of serving it.
	refuse   map[string]int
	requests []string
	bodies   map[string]json.RawMessage
}

func newGitHubFake(t *testing.T, rulesets ...*githubRuleset) *githubFake {
	return &githubFake{t: t, admin: true, rulesets: byID(rulesets...), nextID: 200, refuse: map[string]int{}, bodies: map[string]json.RawMessage{}}
}

func (g *githubFake) writes() []string {
	var out []string
	for _, request := range g.requests {
		if !strings.HasPrefix(request, http.MethodGet) {
			out = append(out, request)
		}
	}
	return out
}

func (g *githubFake) sorted() []*githubRuleset {
	ids := make([]int64, 0, len(g.rulesets))
	for id := range g.rulesets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*githubRuleset, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.rulesets[id])
	}
	return out
}

func (g *githubFake) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if req.Header.Get("Authorization") != "Bearer gh-test-token" {
		g.t.Errorf("GitHub request %s %s without the person's token", req.Method, req.URL.Path)
	}
	key := req.Method + " " + req.URL.Path
	g.requests = append(g.requests, key)
	if len(body) > 0 {
		g.bodies[key] = body
	}
	if status, ok := g.refuse[key]; ok {
		return reportJSONResponse(status, map[string]any{"message": "refused by the fake"}), nil
	}
	const repo = "/repos/acme/app"
	switch {
	case key == "GET "+repo:
		return reportJSONResponse(http.StatusOK, map[string]any{"permissions": map[string]any{"admin": g.admin}}), nil
	case key == "GET "+repo+"/rules/branches/main":
		return reportJSONResponse(http.StatusOK, branchRulesOf(g.sorted()...)), nil
	case key == "GET "+repo+"/rulesets":
		return reportJSONResponse(http.StatusOK, g.sorted()), nil
	case key == "POST "+repo+"/rulesets":
		var write githubRulesetWrite
		_ = json.Unmarshal(body, &write)
		created := &githubRuleset{ID: g.nextID, Name: write.Name, Target: write.Target, Enforcement: write.Enforcement,
			Conditions: write.Conditions, BypassActors: write.BypassActors, Rules: write.Rules}
		g.rulesets[created.ID] = created
		g.nextID++
		return reportJSONResponse(http.StatusCreated, created), nil
	case strings.HasPrefix(key, "GET "+repo+"/rulesets/"), strings.HasPrefix(key, "PUT "+repo+"/rulesets/"):
		id, _ := strconv.ParseInt(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], 10, 64)
		ruleset := g.rulesets[id]
		if ruleset == nil {
			return reportJSONResponse(http.StatusNotFound, map[string]any{"message": "Not Found"}), nil
		}
		if req.Method == http.MethodPut {
			var write githubRulesetWrite
			_ = json.Unmarshal(body, &write)
			ruleset.Name, ruleset.BypassActors, ruleset.Rules = write.Name, write.BypassActors, write.Rules
		}
		return reportJSONResponse(http.StatusOK, ruleset), nil
	}
	return reportJSONResponse(http.StatusNotFound, map[string]any{"message": "no fake for " + key}), nil
}

// queueTransport sends GitHub calls to the GitHub fake and every other call
// to the control-plane fake.
type queueTransport struct {
	control *ciFakeServer
	github  *githubFake
}

func (q queueTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == testGitHubHost {
		return q.github.RoundTrip(req)
	}
	return q.control.RoundTrip(req)
}

type queueRun struct {
	lines   []string
	confirm []string
	err     error
}

func (r queueRun) output() string { return strings.Join(r.lines, "\n") }

// runQueue drives `putnami cloud ci queue …` against both fakes. answer is
// what the person types at the confirmation prompt.
func runQueue(t *testing.T, control *ciFakeServer, github *githubFake, answer string, args ...string) queueRun {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	ioctx, root := newCITestIO(t, control)
	home := t.TempDir()
	ioctx.Env = hometest.Env(home, map[string]string{
		"PUTNAMI_AUTH_URL": reportTestBaseURL,
		"NO_COLOR":         "1",
		"GH_TOKEN":         "gh-test-token",
		"GITHUB_API_URL":   "https://" + testGitHubHost,
	})
	writeReportTestAuth(t, home)
	ioctx.Client = &http.Client{Transport: queueTransport{control: control, github: github}}
	var run queueRun
	ioctx.Stdout = func(line string) { run.lines = append(run.lines, line) }
	ioctx.Confirm = func(question string) (string, bool) {
		run.confirm = append(run.confirm, question)
		return answer, true
	}
	run.err = runCI(ioctx, root, append([]string{"queue"}, args...))
	return run
}

const queuePath = "/v1/workspaces/ws-acme/ci/queue"

func queueSettingStub(enabled bool) ciStubResponse {
	return ciStubResponse{status: http.StatusOK, body: map[string]any{
		"repository": "acme/app", "branch": "main", "configured": enabled, "enabled": enabled, "depth": 1,
	}}
}

func queueControl(enabled bool, put ciStubResponse) *ciFakeServer {
	return &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + queuePath: queueSettingStub(enabled),
		"PUT " + queuePath: put,
	}}
}

func controlWrites(control *ciFakeServer) []ciCapturedRequest {
	var out []ciCapturedRequest
	for _, request := range control.requests {
		if request.Method != http.MethodGet {
			out = append(out, request)
		}
	}
	return out
}

func TestQueueEnableSplitsTheRulesThenTurnsTheQueueOn(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	control := queueControl(false, queueSettingStub(true))
	run := runQueue(t, control, github, "y", "enable")
	if run.err != nil {
		t.Fatalf("enable: %v\n%s", run.err, run.output())
	}
	if want := []string{"POST /repos/acme/app/rulesets", "PUT /repos/acme/app/rulesets/100"}; !slices.Equal(github.writes(), want) {
		t.Fatalf("GitHub writes = %v, want the create before the update", github.writes())
	}
	created := github.rulesets[200]
	if created == nil || created.Name != "main: pull requests" || typesOf(created.Rules) != "pull_request" {
		t.Fatalf("created = %+v", created)
	}
	if typesOf(github.rulesets[100].Rules) != "deletion,non_fast_forward,required_signatures,required_status_checks" {
		t.Fatalf("ruleset 100 rules = %s", typesOf(github.rulesets[100].Rules))
	}
	writes := controlWrites(control)
	if len(writes) != 1 || writes[0].Query != "branch=main" || writes[0].Body["enabled"] != true {
		t.Fatalf("delivery writes = %+v, want one PUT enabled for main", writes)
	}
	if _, asked := writes[0].Body["depth"]; asked {
		t.Fatalf("enable without --depth asked a depth: %v", writes[0].Body)
	}
	if len(run.confirm) != 1 || !strings.Contains(run.confirm[0], "Change the rulesets of acme/app") {
		t.Fatalf("confirm = %v", run.confirm)
	}
	out := run.output()
	for _, want := range []string{"Create ruleset \"main: pull requests\"", "Update ruleset \"main\" (100): drop the pull_request rule.",
		"Created ruleset", "Verified", "on, depth 1", "gh pr merge -R acme/app --auto --squash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output omits %q:\n%s", want, out)
		}
	}
	backup := ""
	for _, line := range run.lines {
		if _, path, ok := strings.Cut(line, "as it was: "); ok {
			backup = path
		}
	}
	data, err := os.ReadFile(backup)
	if err != nil || !strings.Contains(string(data), `"pull_request"`) {
		t.Fatalf("backup %q = %s, %v; want ruleset 100 as it was", backup, data, err)
	}
}

func TestQueueEnableDryRunChangesNothing(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	control := queueControl(false, queueSettingStub(true))
	run := runQueue(t, control, github, "y", "enable", "--dry-run")
	if run.err != nil {
		t.Fatalf("dry run: %v", run.err)
	}
	if len(github.writes()) != 0 || len(controlWrites(control)) != 0 || len(run.confirm) != 0 {
		t.Fatalf("a dry run wrote: GitHub %v, delivery %v, confirm %v", github.writes(), controlWrites(control), run.confirm)
	}
	if !strings.Contains(run.output(), "Dry run: nothing changed.") || !strings.Contains(run.output(), "Create ruleset") {
		t.Fatalf("output:\n%s", run.output())
	}
}

func TestQueueEnableStopsWhenThePersonSaysNo(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	control := queueControl(false, queueSettingStub(true))
	run := runQueue(t, control, github, "n", "enable")
	if run.err == nil || !strings.Contains(run.err.Error(), "aborted") {
		t.Fatalf("err = %v, want aborted", run.err)
	}
	if len(github.writes()) != 0 || len(controlWrites(control)) != 0 {
		t.Fatalf("a declined enable wrote: GitHub %v, delivery %v", github.writes(), controlWrites(control))
	}
}

func TestQueueEnableOnSplitRulesOnlyTurnsTheSettingOn(t *testing.T) {
	main := mainRuleset(t)
	main.Rules = append(main.Rules[:3], main.Rules[4:]...)
	github := newGitHubFake(t, main, splitRuleset(t, 200, "always"))
	control := queueControl(false, queueSettingStub(true))
	run := runQueue(t, control, github, "", "enable", "--yes", "--depth", "1")
	if run.err != nil {
		t.Fatalf("enable: %v", run.err)
	}
	if len(github.writes()) != 0 {
		t.Fatalf("GitHub writes = %v, want none", github.writes())
	}
	writes := controlWrites(control)
	if len(writes) != 1 || writes[0].Body["depth"] != float64(1) {
		t.Fatalf("delivery writes = %+v, want enabled at depth 1", writes)
	}
	if !strings.Contains(run.output(), "ready") {
		t.Fatalf("output:\n%s", run.output())
	}
}

func TestQueueEnableResumesAfterARefusedUpdate(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	github.refuse["PUT /repos/acme/app/rulesets/100"] = http.StatusUnprocessableEntity
	control := queueControl(false, queueSettingStub(true))
	run := runQueue(t, control, github, "y", "enable")
	if run.err == nil || !strings.Contains(run.err.Error(), "delete ruleset 200 to undo") {
		t.Fatalf("err = %v, want the way back", run.err)
	}
	if len(controlWrites(control)) != 0 {
		t.Fatal("the queue turned on although the split failed")
	}
	delete(github.refuse, "PUT /repos/acme/app/rulesets/100")
	github.requests = nil
	run = runQueue(t, control, github, "y", "enable")
	if run.err != nil {
		t.Fatalf("resume: %v\n%s", run.err, run.output())
	}
	if want := []string{"PUT /repos/acme/app/rulesets/100"}; !slices.Equal(github.writes(), want) {
		t.Fatalf("resume writes = %v, want the update only", github.writes())
	}
}

func TestQueueEnableRefusals(t *testing.T) {
	foreign := mainRuleset(t)
	foreign.Rules[4] = rawJSON(t, testChecksRule("Putnami CI", "Contributor CI / gate"))
	for _, tc := range []struct {
		name    string
		github  *githubFake
		put     ciStubResponse
		enabled bool
		want    []string
		wrote   bool
	}{
		{name: "not an admin", github: func() *githubFake { g := newGitHubFake(t, mainRuleset(t)); g.admin = false; return g }(),
			want: []string{"does not administer acme/app"}},
		{name: "a check that never reports", github: newGitHubFake(t, foreign), want: []string{"Contributor CI / gate"}},
		{name: "the create is refused", github: func() *githubFake {
			g := newGitHubFake(t, mainRuleset(t))
			g.refuse["POST /repos/acme/app/rulesets"] = http.StatusUnprocessableEntity
			return g
		}(), want: []string{"refused by the fake", "Nothing changed"}},
		{name: "Delivery refuses", github: newGitHubFake(t, mainRuleset(t)), wrote: true,
			put: ciStubResponse{status: http.StatusConflict, body: map[string]any{
				"code": "conflict", "error": "the queue cannot run on this branch", "reason": "grant it Administration: read",
				"message": "the queue cannot run on this branch: grant it Administration: read",
			}},
			want: []string{"cloud ci queue enable: the queue cannot run on this branch: grant it Administration: read"}},
		{name: "a machine token", github: newGitHubFake(t, mainRuleset(t)), wrote: true,
			put:  ciStubResponse{status: http.StatusForbidden, body: map[string]any{"code": "forbidden", "error": "a person changes the queue setting"}},
			want: []string{"a person changes the queue setting", "PUTNAMI_CLOUD_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := queueControl(tc.enabled, tc.put)
			run := runQueue(t, control, tc.github, "y", "enable")
			if run.err == nil {
				t.Fatalf("enable passed:\n%s", run.output())
			}
			for _, want := range tc.want {
				if !strings.Contains(run.err.Error(), want) {
					t.Fatalf("err = %v, want it to say %q", run.err, want)
				}
			}
			if wrote := len(controlWrites(control)) > 0; wrote != tc.wrote {
				t.Fatalf("delivery writes = %v, want written %v", controlWrites(control), tc.wrote)
			}
		})
	}
}

func TestQueueEnableWhenOnChangesNothing(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	control := queueControl(true, queueSettingStub(true))
	run := runQueue(t, control, github, "y", "enable")
	if run.err != nil || len(github.requests) != 0 || len(controlWrites(control)) != 0 {
		t.Fatalf("err = %v, GitHub %v, delivery %v: want nothing done", run.err, github.requests, controlWrites(control))
	}
	if !strings.Contains(run.output(), "on, depth 1") {
		t.Fatalf("output:\n%s", run.output())
	}
}

func TestQueueStatus(t *testing.T) {
	github := newGitHubFake(t, mainRuleset(t))
	control := queueControl(false, ciStubResponse{})
	run := runQueue(t, control, github, "", "status")
	if run.err != nil {
		t.Fatalf("status: %v", run.err)
	}
	out := run.output()
	for _, want := range []string{"Merge queue of acme/app, branch main: off.", "enable changes them first", "Create ruleset", "putnami cloud ci queue enable`"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output omits %q:\n%s", want, out)
		}
	}
	if len(github.writes()) != 0 || len(controlWrites(control)) != 0 {
		t.Fatal("status wrote")
	}

	run = runQueue(t, control, github, "", "--output=jsonl")
	if run.err != nil || len(run.lines) != 1 {
		t.Fatalf("json status: %v, %d lines", run.err, len(run.lines))
	}
	var envelope struct {
		Data CIQueueResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(run.lines[0]), &envelope); err != nil || envelope.Data.Rules == nil || envelope.Data.Rules.Create == nil ||
		envelope.Data.Setting.Repository != "acme/app" {
		t.Fatalf("json = %s, err = %v", run.lines[0], err)
	}
}

func TestQueueStatusWithoutGitHubCredentials(t *testing.T) {
	saved := githubAuthToken
	t.Cleanup(func() { githubAuthToken = saved })
	githubAuthToken = func(context.Context) (string, error) { return "", fmt.Errorf("no gh") }
	control := queueControl(true, ciStubResponse{})
	ioctx, root := newCITestIO(t, control)
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	if err := runCI(ioctx, root, []string{"queue", "status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "on, depth 1") || !strings.Contains(out, "no GitHub credentials") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestQueueDisable(t *testing.T) {
	github := newGitHubFake(t)
	control := queueControl(true, queueSettingStub(false))
	run := runQueue(t, control, github, "y", "disable")
	if run.err != nil {
		t.Fatalf("disable: %v", run.err)
	}
	writes := controlWrites(control)
	if len(writes) != 1 || writes[0].Body["enabled"] != false || len(github.requests) != 0 {
		t.Fatalf("delivery writes = %+v, GitHub %v: want one PUT disabled and no GitHub call", writes, github.requests)
	}
	if len(run.confirm) != 1 || !strings.Contains(run.confirm[0], "auto-merge") || !strings.Contains(run.output(), "queue-off") {
		t.Fatalf("confirm %v, output:\n%s", run.confirm, run.output())
	}

	control = queueControl(false, queueSettingStub(false))
	run = runQueue(t, control, github, "y", "disable")
	if run.err != nil || len(controlWrites(control)) != 0 || len(run.confirm) != 0 {
		t.Fatalf("disable when off: err %v, writes %v, confirm %v", run.err, controlWrites(control), run.confirm)
	}
}

func TestQueueUsage(t *testing.T) {
	github := newGitHubFake(t)
	for args, want := range map[string]string{
		"merge":                    "unknown cloud ci queue verb",
		"enable --depth 17":        "--depth must be a number from 1 to 16",
		"enable --depth x":         "--depth must be a number",
		"status --app-id nope":     "--app-id must be the numeric id",
		"enable --app-id -3 --yes": "--app-id must be the numeric id",
	} {
		run := runQueue(t, queueControl(false, ciStubResponse{}), github, "", strings.Fields(args)...)
		if run.err == nil || !strings.Contains(run.err.Error(), want) || clicore.ExitCode(run.err) != clicore.ExitUsage {
			t.Fatalf("%s: err = %v, want a usage error saying %q", args, run.err, want)
		}
	}
}
