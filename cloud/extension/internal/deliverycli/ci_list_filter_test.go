package deliverycli

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// The run list filters on the server. These tests pin what the client
// sends, when it stops, and how it falls back to filtering a page itself when
// the page says the server did not apply a filter.

const ciRunsRoute = "GET /v1/workspaces/ws-acme/runs"

// ciListRequests returns the queries of the run list requests the fake saw.
func ciListRequests(t *testing.T, fake *ciFakeServer) []url.Values {
	t.Helper()
	var out []url.Values
	for _, req := range fake.requests {
		if req.Method != "GET" || req.Path != "/v1/workspaces/ws-acme/runs" {
			continue
		}
		q, err := url.ParseQuery(req.Query)
		if err != nil {
			t.Fatalf("parse query %q: %v", req.Query, err)
		}
		out = append(out, q)
	}
	return out
}

func ciHexID(prefix string) string {
	return prefix + strings.Repeat("0", 64-len(prefix))
}

func TestCIListSendsEveryFilterToTheServer(t *testing.T) {
	after := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		ciRunsRoute: {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: ciHexID("a1"), Repo: "acme/app", Branch: "main", SHA: "abc123", PullRequest: 7, Status: "completed", Conclusion: "failure"}},
			Filter: &CIRunListFilter{
				Repo: "acme/app", Branch: "main", Ref: "abc", PullRequest: 7, Status: "failure", Phase: "test",
				CreatedAfter: &after,
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--repo", "acme/app", "--branch", "main", "--sha", "abc",
		"--pr", "7", "--status", "failure", "--phase", "test", "--age", "1h", "--limit", "5"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	requests := ciListRequests(t, fake)
	if len(requests) != 1 {
		t.Fatalf("list sent %d run list requests, want one", len(requests))
	}
	for _, req := range fake.requests {
		if strings.HasPrefix(req.Path, "/v1/workspaces/ws-acme/runs/") {
			t.Fatalf("list read %s although the server applied the phase", req.Path)
		}
	}
	for key, want := range map[string]string{
		"repo": "acme/app", "branch": "main", "ref": "abc", "pullRequest": "7", "status": "failure",
		"phase": "test", "createdAfter": "2026-07-24T08:00:00Z", "limit": "6", "offset": "0",
	} {
		if got := requests[0].Get(key); got != want {
			t.Fatalf("query %s = %q, want %q (whole query %v)", key, got, want, requests[0])
		}
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "a1") {
		t.Fatalf("list did not print the run the server matched:\n%s", out)
	}
}

// A filtered list asks for `limit` runs and one more, and stops there: one
// request, whatever the size of the history behind it. The extra row is what
// says that more runs match.
func TestCIListStopsAtTheLimitTheServerFilled(t *testing.T) {
	failed := func(ids ...string) []CIRunResponse {
		out := make([]CIRunResponse, 0, len(ids))
		for _, id := range ids {
			out = append(out, CIRunResponse{ID: ciHexID(id), Conclusion: "failure"})
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		page       CIRunListResponse
		wantNotice bool
	}{
		{"more match", CIRunListResponse{Runs: failed("b1", "b2", "b3", "b4"), Limit: 4, NextOffset: 4}, true},
		{"exactly the limit", CIRunListResponse{Runs: failed("b1", "b2", "b3"), Limit: 4}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.page.Filter = &CIRunListFilter{Status: "failure"}
			fake := &ciFakeServer{responses: map[string]ciStubResponse{ciRunsRoute: {status: 200, body: tc.page}}}
			lines, err := captureCI(t, fake, []string{"list", "--status", "failure", "--limit", "3"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if requests := ciListRequests(t, fake); len(requests) != 1 || requests[0].Get("limit") != "4" {
				t.Fatalf("list sent %v, want one request for 3 runs and one more", requests)
			}
			out := strings.Join(lines, "\n")
			if tc.wantNotice && (len(lines) != 3+2 || !strings.Contains(lines[len(lines)-1], "more runs match")) {
				t.Fatalf("list printed:\n%s\nwant header + 3 rows + a notice that more runs match", out)
			}
			if !tc.wantNotice && (len(lines) != 3+1 || strings.Contains(out, "more")) {
				t.Fatalf("list printed:\n%s\nwant header + 3 rows and no notice", out)
			}
		})
	}
}

// A page that echoes an empty filter applied nothing: the client filters it.
func TestCIListFiltersAPageThatAppliedNothing(t *testing.T) {
	red := ciHexID("e2")
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		ciRunsRoute: {status: 200, body: CIRunListResponse{
			Runs:   []CIRunResponse{{ID: ciHexID("e1"), Conclusion: "success"}, {ID: red, Conclusion: "failure"}},
			Filter: &CIRunListFilter{},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--status", "failure"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out := strings.Join(lines, "\n"); len(lines) != 1+1 || !strings.Contains(out, red[:ciShortRunIDLength]) {
		t.Fatalf("list printed:\n%s\nwant only the failed run", out)
	}
}

// During a rolling deploy a filtered first page can be followed by a page
// from an older host. The two count offsets differently, so the walk starts
// again from offset 0 with plain pages and filters them itself.
func TestCIListRestartsWhenAPageComesFromTheOtherSideOfADeploy(t *testing.T) {
	red, older := ciHexID("f1"), ciHexID("f3")
	fake := &ciFakeServer{responseSequences: map[string][]ciStubResponse{
		ciRunsRoute: {
			{status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{{ID: red, Conclusion: "failure"}}, NextOffset: 1,
				Filter: &CIRunListFilter{Status: "failure"},
			}},
			{status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{{ID: ciHexID("f2"), Conclusion: "success"}}, NextOffset: 2,
			}},
			{status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{
					{ID: red, Conclusion: "failure"}, {ID: ciHexID("f2"), Conclusion: "success"}, {ID: older, Conclusion: "failure"},
				},
			}},
		},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--status", "failure", "--limit", "0"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	requests := ciListRequests(t, fake)
	if len(requests) != 3 {
		t.Fatalf("list sent %d requests, want 3", len(requests))
	}
	if requests[2].Get("offset") != "0" || requests[2].Get("status") != "" {
		t.Fatalf("third request = %v, want a plain page from offset 0", requests[2])
	}
	out := strings.Join(lines, "\n")
	if len(lines) != 1+2 || !strings.Contains(out, red[:ciShortRunIDLength]) || !strings.Contains(out, older[:ciShortRunIDLength]) {
		t.Fatalf("list printed:\n%s\nwant the two failed runs once each", out)
	}
}

// An older server ignores the filters and says nothing about them. The client
// then filters each page itself and reads the rest of the history in the widest
// pages, as it did before the server filtered.
func TestCIListFiltersItselfAgainstAnOlderServer(t *testing.T) {
	red := ciHexID("c3")
	fake := &ciFakeServer{responseSequences: map[string][]ciStubResponse{
		ciRunsRoute: {
			{status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{
					{ID: ciHexID("c1"), Repo: "acme/app", Conclusion: "success"},
					{ID: ciHexID("c2"), Repo: "acme/lib", Conclusion: "failure"},
				},
				NextOffset: 2,
			}},
			{status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{{ID: red, Repo: "acme/app", Conclusion: "failure"}},
			}},
		},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--repo", "acme/app", "--status", "failure", "--limit", "2"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	requests := ciListRequests(t, fake)
	if len(requests) != 2 {
		t.Fatalf("list sent %d requests, want 2", len(requests))
	}
	if requests[0].Get("limit") != "3" || requests[0].Get("repo") != "acme/app" {
		t.Fatalf("first page = %v, want the filtered ask for 2 runs and one more", requests[0])
	}
	if requests[1].Get("limit") != "200" || requests[1].Get("offset") != "2" || requests[1].Get("repo") != "" || requests[1].Get("status") != "" {
		t.Fatalf("second page = %v, want the widest plain page from offset 2", requests[1])
	}
	out := strings.Join(lines, "\n")
	if len(lines) != 1+1 || !strings.Contains(out, red[:ciShortRunIDLength]) {
		t.Fatalf("list printed:\n%s\nwant only the red acme/app run", out)
	}
}

// The filter member is read per member: a server that applied the status but
// not the phase leaves the client to check the phase on each run it returned.
func TestCIListChecksAPhaseTheServerDidNotApply(t *testing.T) {
	tested, linted := ciHexID("d1"), ciHexID("d2")
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		ciRunsRoute: {status: 200, body: CIRunListResponse{
			Runs:   []CIRunResponse{{ID: tested, Conclusion: "failure"}, {ID: linted, Conclusion: "failure"}},
			Filter: &CIRunListFilter{Status: "failure"},
		}},
		"GET /v1/workspaces/ws-acme/runs/" + tested: {status: 200, body: CIRunResponse{ID: tested, Phases: []CIRunPhaseResponse{{Phase: "test"}}}},
		"GET /v1/workspaces/ws-acme/runs/" + linted: {status: 200, body: CIRunResponse{ID: linted, Phases: []CIRunPhaseResponse{{Phase: "lint"}}}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--status", "failure", "--phase", "test"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, tested[:ciShortRunIDLength]) || strings.Contains(out, linted[:ciShortRunIDLength]) {
		t.Fatalf("list printed:\n%s\nwant only the run with a test phase", out)
	}
}

func TestCIListFilterServerQuery(t *testing.T) {
	now := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	q := ciListFilters{pr: 0, hasPR: true}.serverQuery(now)
	if q.PullRequest != nil {
		t.Fatalf("--pr 0 was sent as pullRequest=%d; it has no server form", *q.PullRequest)
	}
	if q := (ciListFilters{}).serverQuery(now); q.Repo != nil || q.Ref != nil || q.Status != nil || q.CreatedAfter != nil {
		t.Fatalf("no filter built a filtered query: %+v", q)
	}

	asked := ciListFilters{repo: "acme/app", pr: 0, hasPR: true, status: "failure", age: time.Hour, hasAge: true}
	if got := asked.pendingAfter(nil); got != asked {
		t.Fatalf("a page without a filter member left %+v pending, want everything", got)
	}
	after := now.Add(-time.Hour)
	got := asked.pendingAfter(&CIRunListFilter{Repo: "acme/app", Status: "failure", CreatedAfter: &after})
	if want := (ciListFilters{pr: 0, hasPR: true}); got != want {
		t.Fatalf("pending = %+v, want only the pull request the server cannot take", got)
	}
}

// A prefix costs one request: the server returns at most the two runs that
// start with it.
func TestCIRunPrefixAsksTheServer(t *testing.T) {
	full := ciHexID("abcd1")
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		ciRunsRoute: {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: full}}, Filter: &CIRunListFilter{IDPrefix: "abcd1"},
		}},
		"GET /v1/workspaces/ws-acme/runs/" + full: {status: 200, body: CIRunResponse{ID: full, Status: "completed"}},
	}}
	if _, err := captureCI(t, fake, []string{"view", "ABCD1"}); err != nil {
		t.Fatalf("view by prefix: %v", err)
	}
	requests := ciListRequests(t, fake)
	if len(requests) != 1 || requests[0].Get("idPrefix") != "abcd1" || requests[0].Get("limit") != "2" {
		t.Fatalf("prefix resolution sent %v, want one request for two runs with idPrefix=abcd1", requests)
	}
	if got := lastCIRequest(t, fake); got.Path != "/v1/workspaces/ws-acme/runs/"+full {
		t.Fatalf("final request = %s, want the resolved by-id route", got.Path)
	}
}

// Against an older server the client walks the history itself, and stops at
// the second match: the prefix is ambiguous by then, whatever lies further back.
func TestCIRunPrefixFallbackStopsAtTheSecondMatch(t *testing.T) {
	page := make([]CIRunResponse, 0, ciListPageLimit)
	page = append(page, CIRunResponse{ID: ciHexID("ee01")}, CIRunResponse{ID: ciHexID("ee02")})
	fake := &ciFakeServer{responseSequences: map[string][]ciStubResponse{
		ciRunsRoute: {
			{status: 200, body: CIRunListResponse{Runs: []CIRunResponse{{ID: ciHexID("ff")}, {ID: ciHexID("ab")}}, NextOffset: 2}},
			{status: 200, body: CIRunListResponse{Runs: page, NextOffset: 202}},
			{status: 200, body: CIRunListResponse{Runs: []CIRunResponse{{ID: ciHexID("ee03")}}}},
		},
	}}
	_, err := captureCI(t, fake, []string{"view", "ee0"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("view ee0 = %v, want an ambiguity refusal", err)
	}
	requests := ciListRequests(t, fake)
	if len(requests) != 2 {
		t.Fatalf("prefix resolution sent %d run list requests, want 2 (it must stop at the second match)", len(requests))
	}
	if requests[0].Get("idPrefix") != "ee0" || requests[1].Get("idPrefix") != "" || requests[1].Get("offset") != "2" {
		t.Fatalf("requests = %v, want the idPrefix ask, then the plain walk from offset 2", requests)
	}
}

// When the prefix lookup itself fails, the by-id route's own answer stands:
// the caller sees the same error as for a prefix that matches nothing.
func TestCIRunPrefixKeepsTheOriginalErrorWhenTheLookupFails(t *testing.T) {
	view := func(list ciStubResponse) error {
		fake := &ciFakeServer{responses: map[string]ciStubResponse{ciRunsRoute: list}}
		_, err := captureCI(t, fake, []string{"view", "abcd"})
		return err
	}
	noMatch := view(ciStubResponse{status: 200, body: CIRunListResponse{Filter: &CIRunListFilter{IDPrefix: "abcd"}}})
	failed := view(ciStubResponse{status: 500, body: map[string]any{"error": "list backend down"}})
	if noMatch == nil || failed == nil {
		t.Fatalf("view of an unknown prefix succeeded: no match = %v, failed lookup = %v", noMatch, failed)
	}
	if failed.Error() != noMatch.Error() {
		t.Fatalf("a failed lookup answered %q, want the by-id error %q", failed, noMatch)
	}
}

// A walk the server filtered stops at the page cap like any other, and says
// it read matching runs, counting the rows it actually read.
func TestCIListSaysWhenAFilteredWalkWasCapped(t *testing.T) {
	page := make([]CIRunResponse, 0, ciListPageLimit)
	for i := 0; i < ciListPageLimit; i++ {
		page = append(page, CIRunResponse{ID: ciHexID("ab"), Conclusion: "failure"})
	}
	// NextOffset always advances, so the walk can only end at the page cap.
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		ciRunsRoute: {status: 200, body: CIRunListResponse{Runs: page, NextOffset: 1, Filter: &CIRunListFilter{Status: "failure"}}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--status", "failure", "--limit", "0"})
	if err != nil {
		t.Fatalf("list --limit 0: %v", err)
	}
	want := "stopped after 4000 matching runs (20 pages)"
	if last := lines[len(lines)-1]; !strings.Contains(last, want) {
		t.Fatalf("the capped walk said %q, want %q", last, want)
	}
}
