package provider

import (
	"net/http"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

func TestAMissingItemIsNotFound(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-missing-item-is-not-found")
	h := newHarness(t)
	for _, ref := range []string{
		`{"source":"jira:acme.atlassian.net","id":"99999"}`,
		`{"source":"jira:other.atlassian.net","id":"10000"}`,
		`{"source":"github:acme/app","id":"1"}`,
		`{"source":"jira:acme.atlassian.net","id":"ENG-1"}`,
		`{"source":"jira:acme.atlassian.net","id":"010000"}`,
	} {
		h.fails("get", `{"ref":`+ref+`}`, collab.OutcomeNotFound)
		h.fails("update", `{"ref":`+ref+`,"title":"x"}`, collab.OutcomeNotFound)
		h.fails("transition", `{"ref":`+ref+`,"state":"done"}`, collab.OutcomeNotFound)
	}
	issue := h.seed("Present", "")
	h.ok("get", `{"ref":{"source":"jira:ACME.atlassian.net","id":"`+strings.TrimPrefix(h.ref(issue), `{"source":"jira:acme.atlassian.net","id":"`)+`}`, &collab.TaskResult{})
}

func TestARateLimitIsRetryableAndNeverRepeated(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-rate-limit-is-retryable-and-never-repeated")
	h := newHarness(t)
	issue := h.seed("Limited", "")
	fault := statusFault(http.StatusTooManyRequests, "Rate limit exceeded.", true)
	fault.header = map[string]string{"Retry-After": "12"}
	h.fake.addFault("GET", `^/rest/api/2/issue/`, fault)
	failure := h.fails("get", `{"ref":`+h.ref(issue)+`}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonRateLimited || h.fake.count("GET /rest/api/2/issue/") != 1 {
		t.Fatalf("a rate limit answered %+v after %d reads", failure, h.fake.count("GET /rest/api/2/issue/"))
	}
	mustContain(t, failure.Message, "retry after 12s")
}

func TestAnOutageIsUnavailableAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "an-outage-is-unavailable-and-writes-nothing")
	h := newHarness(t)
	h.fake.addFault("POST", `^/rest/api/2/search/jql$`, fault{status: http.StatusServiceUnavailable, before: true, body: `{}`, times: 3})
	failure := h.fails("create", `{"title":"During an outage","idempotencyKey":"outage"}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonKeyUnverifiable || h.fake.count("POST /rest/api/2/issue") != 0 || h.issueCount() != 0 {
		t.Fatalf("an outage answered %+v", failure)
	}
}

func TestARefusedCredentialIsDenied(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-refused-credential-is-denied")
	h := newHarness(t)
	h.env[jira.TokenVariable] = "ATATT3xWRONGtokenWRONGtokenWRONG00"
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthorized {
		t.Fatalf("%+v", failure)
	}
	h = newHarness(t)
	h.fake.addFault("GET", `^/rest/api/2/issue/`, statusFault(http.StatusForbidden, "You do not have permission to edit issues in this project.", true))
	if failure := h.fails("get", `{"ref":`+h.ref(h.seed("x", ""))+`}`, collab.OutcomeDenied); failure.Reason != reasonForbidden {
		t.Fatalf("%+v", failure)
	}
}

func TestAStaleRevisionIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-stale-revision-is-a-conflict")
	h := newHarness(t)
	issue := h.seed("Stale", "")
	stale := h.get(issue).Revision
	h.fake.mu.Lock()
	issue.summary = "Changed by someone"
	h.fake.mu.Unlock()
	current := h.get(issue).Revision
	for _, arguments := range []string{
		`{"ref":` + h.ref(issue) + `,"title":"Mine","expectedRevision":"` + stale + `"}`,
	} {
		failure := h.fails("update", arguments, collab.OutcomeConflict)
		if failure.Reason != collab.ReasonRevisionConflict || failure.Current != current {
			t.Fatalf("%+v", failure)
		}
	}
	if failure := h.fails("transition", `{"ref":`+h.ref(issue)+`,"state":"done","expectedRevision":"`+stale+`"}`, collab.OutcomeConflict); failure.Current != current {
		t.Fatalf("%+v", failure)
	}
	if h.fake.count("PUT ") != 0 || h.fake.count("POST /rest/api/2/issue/") != 0 {
		t.Fatal("a stale write was sent")
	}
}

func TestSettingsAreStrict(t *testing.T) {
	h := newHarness(t)
	for _, settings := range []string{
		`{}`,
		`{"site":"acme.atlassian.net"}`,
		`{"site":"https://acme.atlassian.net","project":"ENG"}`,
		`{"site":"acme.atlassian.net","project":"eng"}`,
		`{"site":"acme.atlassian.net","project":"ENG","token":"x"}`,
		`{"site":"acme.atlassian.net","project":"ENG","issueType":" Task"}`,
	} {
		h.settings = settings
		if failure := h.fails("find", `{}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
			t.Fatalf("%s: %+v", settings, failure)
		}
	}
	h.settings = `{"site":"acme.atlassian.net","project":"ENG","issueType":"Bug"}`
	failure := h.fails("create", `{"title":"Typed","idempotencyKey":"typed"}`, collab.OutcomeInvalid)
	mustContain(t, failure.Message, "issuetype: Specify a valid issue type")
}
