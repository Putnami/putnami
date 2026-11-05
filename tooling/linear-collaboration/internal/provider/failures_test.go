package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/linear-collaboration/internal/linear"
)

func TestAMissingItemIsNotFound(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-missing-item-is-not-found")
	h := newHarness(t)
	h.fails("get", `{"ref":{"source":"linear:acme","id":"dddddddd-0000-4000-8000-000000000001"}}`, collab.OutcomeNotFound)
	reads := h.fake.count("PutnamiIssue")
	h.fails("get", `{"ref":{"source":"linear:acme","id":"ENG-1"}}`, collab.OutcomeNotFound)
	h.fails("update", `{"ref":{"source":"linear:other","id":"dddddddd-0000-4000-8000-000000000001"},"title":"x"}`, collab.OutcomeNotFound)
	if h.fake.count("PutnamiIssue") != reads {
		t.Fatal("a reference this organization did not issue was read")
	}
}

func TestLostCreateAnswerIsReadBackByID(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "a-lost-create-answer-is-read-back-by-id")
	h := newHarness(t)
	h.fake.inject("PutnamiCreateIssue", fault{drop: true})
	var created collab.TaskCreateResult
	h.ok("create", `{"title":"Lost answer","idempotencyKey":"lost"}`, &created)
	if !created.Created || created.Task.Ref.ID != keyedID(teamID, "lost") || h.issueCount() != 1 || h.fake.count("PutnamiCreateIssue") != 1 {
		t.Fatalf("after a lost answer: %+v, %d issues, %d creates", created, h.issueCount(), h.fake.count("PutnamiCreateIssue"))
	}
}

func TestAnUnestablishedCreateIsUnresolvedAndSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "an-unestablished-create-is-unresolved-and-sent-once")
	h := newHarness(t)
	h.fake.inject("PutnamiCreateIssue", fault{before: true, status: 502, body: "bad gateway"})
	failure := h.fails("create", `{"title":"Unknown","idempotencyKey":"unknown"}`, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" || h.fake.count("PutnamiCreateIssue") != 1 || h.issueCount() != 0 {
		t.Fatalf("an unestablished create: %+v, %d creates", failure, h.fake.count("PutnamiCreateIssue"))
	}
	var repeat collab.TaskCreateResult
	h.ok("create", `{"title":"Unknown","idempotencyKey":"unknown"}`, &repeat)
	if !repeat.Created || h.issueCount() != 1 {
		t.Fatalf("the repeat the reconcile names answered %+v", repeat)
	}
}

func TestALostUpdateAnswerIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "an-outage-is-unavailable-and-writes-nothing")
	h := newHarness(t)
	task := h.createTask(`{"title":"Lost update","idempotencyKey":"lost-update"}`)
	h.fake.inject("PutnamiUpdateIssue", fault{drop: true})
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+refJSON(task.Ref)+`,"state":"done"}`, &moved)
	if moved.Task.State != collab.TaskStateDone || h.fake.count("PutnamiUpdateIssue") != 1 {
		t.Fatalf("after a lost transition answer: %+v, %d writes", moved.Task, h.fake.count("PutnamiUpdateIssue"))
	}
	h.fake.inject("PutnamiUpdateIssue", fault{before: true, drop: true})
	failure := h.fails("update", `{"ref":`+refJSON(task.Ref)+`,"title":"Never"}`, collab.OutcomeUnresolved)
	if failure.Retryable || h.stored(task.Ref).title != "Lost update" {
		t.Fatalf("an unestablished update: %+v", failure)
	}
}

func TestPagesFollowLinearCursorsExactlyOnce(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "pages-follow-linear-cursors-exactly-once")
	h := newHarness(t)
	created := make([]collab.Ref, 0, 5)
	for i := range 5 {
		created = append(created, h.createTask(`{"title":"Paged `+strconv.Itoa(i)+`","labels":["paged"],"idempotencyKey":"page-`+strconv.Itoa(i)+`"}`).Ref)
	}
	var refs []collab.Ref
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		arguments := `{"labels":["paged"],"page":{"size":2}}`
		if cursor != "" {
			arguments = `{"labels":["paged"],"page":{"size":2,"cursor":` + quote(cursor) + `}}`
		}
		var page collab.TaskListResult
		h.ok("find", arguments, &page)
		for _, task := range page.Items {
			refs = append(refs, task.Ref)
		}
		if page.Page.Next != "" && !strings.HasPrefix(page.Page.Next, cursorPrefix) {
			t.Fatalf("cursor %q", page.Page.Next)
		}
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	if !slices.Equal(refs, created) {
		t.Fatalf("pages listed %v, want %v", refs, created)
	}
	failure := h.fails("find", `{"page":{"size":2,"cursor":"p2.n9"}}`, collab.OutcomeInvalid)
	if failure.Reason != collab.ReasonRequestInvalid {
		t.Fatalf("a foreign cursor: %+v", failure)
	}
}

func TestAnOutageIsUnavailableAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "an-outage-is-unavailable-and-writes-nothing")
	h := newHarness(t)
	h.fake.server.Close()
	failure := h.fails("create", `{"title":"Offline","idempotencyKey":"offline"}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonUnreachable {
		t.Fatalf("an outage: %+v", failure)
	}
}

func TestARateLimitIsRetryableAndNeverRepeated(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-rate-limit-is-retryable-and-never-repeated")
	h := newHarness(t)
	h.fake.inject("PutnamiIssue", statusFault(400, "Rate limit exceeded", "RATELIMITED", true))
	failure := h.fails("get", `{"ref":{"source":"linear:acme","id":"eeeeeeee-0000-4000-8000-000000000001"}}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonRateLimited || h.fake.count("PutnamiIssue") != 1 {
		t.Fatalf("a rate limit: %+v after %d reads", failure, h.fake.count("PutnamiIssue"))
	}
}

func TestARefusedCredentialIsDenied(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-refused-credential-is-denied")
	h := newHarness(t)
	h.env[linear.KeyVariable] = "lin_api_revokedREVOKEDrevokedREVOKED00"
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthorized {
		t.Fatalf("a refused key: %+v", failure)
	}
}

func TestACredentialNeverReachesAnAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	h := newHarness(t)
	const stranger = "lin_api_strangerSTRANGERstrangerSTRANGER"
	h.fake.inject("PutnamiIssue", statusFault(400, "Key "+fakeKey+" and "+stranger+" refused", "INVALID_INPUT", true))
	text := h.serve("get", `{"ref":{"source":"linear:acme","id":"ffffffff-0000-4000-8000-000000000001"}}`)
	if strings.Contains(text, fakeKey) || strings.Contains(text, stranger) || !strings.Contains(text, collabhelper.RedactedMarker) {
		t.Fatalf("the answer carries a key: %s", text)
	}
}

func TestTheKeyComesFromLinearAPIKeyOnly(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-key-comes-from-linear-api-key-only")
	h := newHarness(t)
	delete(h.env, linear.KeyVariable)
	h.env["LINEAR_TOKEN"] = fakeKey
	h.env["LINEAR_ACCESS_TOKEN"] = fakeKey
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthenticated || h.fake.count("PutnamiServed") != 0 {
		t.Fatalf("without LINEAR_API_KEY: %+v after %d requests", failure, h.fake.count("PutnamiServed"))
	}
	h.env[linear.KeyVariable] = fakeKey
	h.settings = `{"team":"ENG","apiKey":"` + fakeKey + `"}`
	if failure := h.fails("find", `{}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
		t.Fatalf("a key in the settings: %+v", failure)
	}
	h.env[linear.KeyVariable] = "two words"
	h.settings = `{"team":"ENG"}`
	h.provider = h.newProvider()
	if failure := h.fails("find", `{}`, collab.OutcomeDenied); failure.Reason != reasonUnauthenticated {
		t.Fatalf("a key that is not one token: %+v", failure)
	}
}

func TestTheOverrideReachesOnlyTheLoopbackInterface(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-override-reaches-only-the-loopback-interface")
	for _, override := range []string{"https://linear.example.com/graphql", "http://user:pass@127.0.0.1/graphql", "ftp://127.0.0.1/", "http://127.0.0.1/graphql?x=1"} {
		if _, err := linear.ResolveEndpoint(func(string) string { return override }); err == nil {
			t.Errorf("override %s was accepted", override)
		}
	}
	endpoint, err := linear.ResolveEndpoint(func(string) string { return "" })
	if err != nil || endpoint.String() != linear.DefaultEndpoint {
		t.Fatalf("the default endpoint is %v (%v)", endpoint, err)
	}
	h := newHarness(t)
	h.env[linear.OverrideVariable] = "https://linear.example.com/graphql"
	if failure := h.fails("find", `{}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
		t.Fatalf("a remote override: %+v", failure)
	}
}

func TestAnUnknownTeamIsASettingsError(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-missing-item-is-not-found")
	h := newHarness(t)
	h.settings = `{"team":"NOPE"}`
	if failure := h.fails("find", `{}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
		t.Fatalf("an unknown team: %+v", failure)
	}
	h.settings = `{}`
	h.fails("find", `{}`, collab.OutcomeInvalid)
}

func TestTheMarkerSurvivesLinearsEscapes(t *testing.T) {
	spectest.Proves(t, feature, "idempotent-create", "update-keeps-the-recorded-digest")
	sum := strings.Repeat("ab", 32)
	for _, body := range []string{
		"Text.\n\n<!-- `putnami-idempotency d=" + sum + "` -->",
		"Text.\n\n<!-- `putnami-idempotency d=" + sum + "` \\-->\n",
		"Text.\n\n\\<!-- `putnami-idempotency d=" + sum + "` -->",
	} {
		text, recorded := splitMarker(body)
		if text != "Text." || recorded != sum {
			t.Errorf("%q split as %q and %q", body, text, recorded)
		}
	}
	if text, recorded := splitMarker("Only text."); text != "Only text." || recorded != "" {
		t.Errorf("a body without a marker split as %q and %q", text, recorded)
	}
}
