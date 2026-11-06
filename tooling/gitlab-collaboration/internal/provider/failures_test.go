package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

func TestAMissingItemIsNotFound(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-missing-item-is-not-found")
	h := newHarness(t)
	for _, ref := range []collab.Ref{
		refOf(404),
		{Source: "gitlab:" + fakeHost + "/acme/other", ID: "1"},
		{Source: "github:acme/app", ID: "1"},
		{Source: source, ID: "01"},
		{Source: source, ID: "x"},
	} {
		for _, operation := range []string{"get", "update", "transition", "assign"} {
			arguments := map[string]string{
				"get": `{"ref":` + refJSON(ref) + `}`, "update": `{"ref":` + refJSON(ref) + `,"title":"x"}`,
				"transition": `{"ref":` + refJSON(ref) + `,"state":"done"}`, "assign": `{"ref":` + refJSON(ref) + `,"assignees":[]}`,
			}[operation]
			h.fails(operation, arguments, collab.OutcomeNotFound)
		}
	}
	failure := h.fails("get", `{"ref":{"source":"gitlab:GITLAB.EXAMPLE.COM/acme/platform/app","id":"1"}}`, collab.OutcomeNotFound)
	mustContain(t, failure.Message, "this provider issues the source as "+source)

	h.settings = `{"project":"acme/missing","host":"` + fakeHost + `"}`
	failure = h.fails("find", `{}`, collab.OutcomeNotFound)
	mustContain(t, failure.Message, "404 Project Not Found")
	if h.fake.writes() != 0 {
		t.Fatal("a missing item was written")
	}
}

func TestARateLimitIsRetryableAndNeverRepeated(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-rate-limit-is-retryable-and-never-repeated")
	h := newHarness(t)
	issue := h.seed("Limited", "")
	h.fake.addFault("GET", `/issues/[0-9]+$`, fault{status: 429, body: `{"message":"Retry later"}`, before: true,
		header: map[string]string{"Retry-After": "30"}})
	before := h.fake.count("GET ")
	failure := h.fails("get", `{"ref":`+refJSON(refOf(issue.iid))+`}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonRateLimited || h.fake.count("GET ")-before != 1 {
		t.Fatalf("a rate limit: %+v after %d reads", failure, h.fake.count("GET ")-before)
	}
	mustContain(t, failure.Message, "retry after 30s")
	h.fake.addFault("POST", `/issues$`, fault{status: 429, body: `{"message":"Retry later"}`, before: true,
		header: map[string]string{"RateLimit-ResetTime": "Tue, 24 Sep 2026 08:01:00 GMT"}})
	failure = h.fails("create", keyedCreate, collab.OutcomeUnavailable)
	if !failure.Retryable || h.fake.count("POST ") != 1 || h.issueCount() != 1 {
		t.Fatalf("a rate-limited create: %+v", failure)
	}
	mustContain(t, failure.Message, "resets at Tue, 24 Sep 2026")
}

func TestARefusedCredentialIsDenied(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-refused-credential-is-denied")
	h := newHarness(t)
	issue := h.seed("Guarded", "")
	ref := refJSON(refOf(issue.iid))
	h.fake.addFault("PUT", `/issues/[0-9]+$`, statusFault(403, "403 Forbidden", true))
	if failure := h.fails("update", `{"ref":`+ref+`,"title":"x"}`, collab.OutcomeDenied); failure.Reason != reasonForbidden {
		t.Fatalf("a 403: %+v", failure)
	}
	h.env[gitlab.TokenVariable] = "glpat-WRONGtokenWRONGtoken9999"
	h.provider = &Provider{Getenv: h.provider.Getenv, Client: h.provider.Client}
	failure := h.fails("get", `{"ref":`+ref+`}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthorized {
		t.Fatalf("a 401: %+v", failure)
	}
	mustContain(t, failure.Message, "the token came from GITLAB_TOKEN")
}

func TestARefusalNamesTheCredentialsOrigin(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-refusal-names-the-credentials-origin")
	h := newHarness(t)
	issue := h.seed("Guarded", "")
	delete(h.env, gitlab.TokenVariable)
	h.env[gitlab.CIVariable], h.env[gitlab.CIHostVariable], h.env[gitlab.JobKeyVariable] = "true", fakeHost, fakeJobToken
	h.fake.addFault("PUT", `/issues/[0-9]+$`, statusFault(403, "403 Forbidden", true))
	failure := h.fails("update", `{"ref":`+refJSON(refOf(issue.iid))+`,"title":"x"}`, collab.OutcomeDenied)
	if failure.Reason != reasonForbidden || strings.Contains(failure.Message, fakeJobToken) {
		t.Fatalf("a refused job token: %+v", failure)
	}
	mustContain(t, failure.Message, "the token came from CI_JOB_TOKEN")
}

func TestAnOutageIsUnavailableAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "an-outage-is-unavailable-and-writes-nothing")
	h := newHarness(t)
	issue := h.seed("Seeded", "")
	ref := refJSON(refOf(issue.iid))

	// A read answered 5xx is repeated, up to three times in all.
	h.fake.addFault("GET", `/issues/[0-9]+$`, fault{status: 503, body: `{"message":"503 Service Unavailable"}`, before: true, times: 2})
	if task := h.get(refOf(issue.iid)); task.Title != "Seeded" {
		t.Fatalf("after two 503 the read answered %+v", task)
	}
	h.fake.addFault("GET", `/issues/[0-9]+$`, fault{status: 503, body: `{"message":"503 Service Unavailable"}`, before: true, times: 3})
	failure := h.fails("update", `{"ref":`+ref+`,"title":"x"}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonUnavailable || h.fake.writes() != 0 {
		t.Fatalf("an outage before the write: %+v", failure)
	}
	// A refused write is invalid, with GitLab's field errors.
	h.fake.addFault("PUT", `/issues/[0-9]+$`, fault{status: 400, body: `{"message":{"title":["can't be blank"]}}`, before: true})
	failure = h.fails("update", `{"ref":`+ref+`,"title":"Refused"}`, collab.OutcomeInvalid)
	mustContain(t, failure.Message, "title can't be blank")
	if h.issue(issue.iid).title != "Seeded" {
		t.Fatal("a refused write changed the issue")
	}
	h.fake.addFault("PUT", `/issues/[0-9]+$`, statusFault(409, "Conflict", true))
	if failure := h.fails("update", `{"ref":`+ref+`,"title":"y"}`, collab.OutcomeConflict); failure.Reason != reasonGitLabConflict {
		t.Fatalf("a 409: %+v", failure)
	}

	// GitLab is unreachable: nothing left the process.
	h.fake.server.Close()
	failure = h.fails("create", keyedCreate, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonUnreachable {
		t.Fatalf("an unreachable GitLab: %+v", failure)
	}
}

func TestAMissingCredentialIsDeniedAndSendsNothing(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-missing-credential-is-denied-and-sends-nothing")
	h := newHarness(t)
	delete(h.env, gitlab.TokenVariable)
	h.provider.TokenCommand = func(context.Context, string) (string, error) { return "", errors.New("glab config get token failed") }
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthenticated || h.fake.count("") != 0 {
		t.Fatalf("no credential: %+v after %d requests", failure, h.fake.count(""))
	}
	mustContain(t, failure.Message, "set GITLAB_TOKEN or run glab auth login")
}

func TestTheCredentialFollowsGlabDiscovery(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-credential-follows-glab-discovery")
	h := newHarness(t)
	// A token for another instance is not sent: glab's stored token is.
	h.env[gitlab.TokenVariable] = "glpat-OTHERinstanceTOKEN0000"
	h.env[gitlab.HostVariable] = "https://gitlab.other.example/"
	var asked []string
	h.provider.TokenCommand = func(_ context.Context, host string) (string, error) {
		asked = append(asked, host)
		return " " + fakeToken + "\n", nil
	}
	var page collab.TaskListResult
	h.ok("find", `{}`, &page)
	h.ok("find", `{}`, &page)
	if len(asked) != 1 || asked[0] != fakeHost {
		t.Fatalf("glab was asked for %v; the credential resolves once per instance", asked)
	}
}

func TestAJobTokenServesOnlyItsOwnInstance(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-job-token-serves-only-its-own-instance")
	h := newHarness(t)
	delete(h.env, gitlab.TokenVariable)
	h.env[gitlab.CIVariable] = "true"
	h.env[gitlab.JobKeyVariable] = fakeJobToken
	h.env[gitlab.CIHostVariable] = "gitlab.other.example"
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	mustContain(t, failure.Message, "the job runs on another instance")
	if strings.Contains(failure.Message, fakeJobToken) || h.fake.count("") != 0 {
		t.Fatalf("a job token of another instance: %+v", failure)
	}

	h.env[gitlab.CIHostVariable] = fakeHost
	h.provider = &Provider{Getenv: h.provider.Getenv, Client: h.provider.Client}
	var page collab.TaskListResult
	h.ok("find", `{}`, &page)
}

func TestACredentialNeverReachesAnAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	h := newHarness(t)
	issue := h.seed("Echo "+fakeToken, "and glpat-SOMEONEelsesTOKEN12345 in the body")
	h.fake.addFault("PUT", `/issues/[0-9]+$`, statusFault(400, "token "+fakeToken+" is "+fakeToken, true))
	text := h.serve("update", `{"ref":`+refJSON(refOf(issue.iid))+`,"title":"x"}`)
	if strings.Contains(text, fakeToken) || !strings.Contains(text, collabhelper.RedactedMarker) {
		t.Fatalf("an error answer carries the credential: %s", text)
	}
	text = h.serve("get", `{"ref":`+refJSON(refOf(issue.iid))+`}`)
	if strings.Contains(text, fakeToken) || strings.Contains(text, "glpat-SOMEONE") {
		t.Fatalf("an answer carries a token: %s", text)
	}
}
