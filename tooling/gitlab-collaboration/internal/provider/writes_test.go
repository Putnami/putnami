package provider

import (
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const keyedCreate = `{"title":"Keyed","body":"Body","labels":["b","a"],"idempotencyKey":"key-1"}`

func TestARepeatedKeyReturnsTheIssueItCreated(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-repeated-key-returns-the-issue-it-created")
	h := newHarness(t)
	first := h.create(keyedCreate)
	if !first.Created || first.Task.Body != "Body" || !slices.Equal(first.Task.Labels, []string{"a", "b"}) {
		t.Fatalf("created %+v", first)
	}
	stored := h.issue(mustIID(t, first.Task.Ref))
	mustContain(t, stored.description, "<!-- putnami-idempotency k=")
	// Labels are a set: their order does not change the request.
	repeat := h.create(`{"title":"Keyed","body":"Body","labels":["a","b"],"idempotencyKey":"key-1"}`)
	if repeat.Created || repeat.Task.Ref != first.Task.Ref {
		t.Fatalf("a repeat answered %+v", repeat)
	}
	if count := h.issueCount(); count != 1 {
		t.Fatalf("a repeat opened %d issues", count)
	}

	// Past the scanned pages, GitLab's search finds the key.
	h.settings = strings.Replace(referenceSettings, `"stateLabelPrefix"`, `"keyScanPages":1,"stateLabelPrefix"`, 1)
	for i := 0; i < 120; i++ {
		h.fake.mu.Lock()
		h.fake.newIssue("Newer", "", nil, botID)
		h.fake.mu.Unlock()
	}
	searches := h.fake.count("GET /api/v4/projects/")
	late := h.create(keyedCreate)
	if late.Created || late.Task.Ref != first.Task.Ref {
		t.Fatalf("a repeat past the scanned pages answered %+v", late)
	}
	if h.fake.count("GET /api/v4/projects/")-searches != 2 {
		t.Fatalf("the lookup did not read one page then the search")
	}
}

func TestAReusedKeyWithOtherContentIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-reused-key-with-other-content-is-a-conflict")
	h := newHarness(t)
	h.create(keyedCreate)
	for _, other := range []string{
		`{"title":"Other","body":"Body","labels":["a","b"],"idempotencyKey":"key-1"}`,
		`{"title":"Keyed","body":"Body","labels":["a"],"idempotencyKey":"key-1"}`,
		`{"title":"Keyed","body":"Body","labels":["a","b"],"state":"in_progress","idempotencyKey":"key-1"}`,
	} {
		failure := h.fails("create", other, collab.OutcomeConflict)
		if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Retryable {
			t.Fatalf("%s: %+v", other, failure)
		}
	}
	if count := h.issueCount(); count != 1 {
		t.Fatalf("a reused key opened %d issues", count)
	}
}

func TestOnlyTheAccountsIssueAnswersForAKey(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "only-the-accounts-issue-answers-for-a-key")
	h := newHarness(t)
	first := h.create(keyedCreate)
	copied := h.issue(mustIID(t, first.Task.Ref)).description
	h.fake.mu.Lock()
	h.fake.issues[mustIID(t, first.Task.Ref)].deleted = true
	h.fake.mu.Unlock()
	h.seed("Copy", copied)
	created := h.create(keyedCreate)
	if !created.Created {
		t.Fatalf("another account's copy answered for the key: %+v", created)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, call := range h.fake.calls {
		if strings.Contains(call, "/issues") && strings.HasPrefix(call, "GET") {
			return
		}
	}
	t.Fatal("the lookup listed no issue")
}

func TestALostCreateResponseReturnsTheIssueWithoutADuplicate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-create-response-returns-the-issue-without-a-duplicate")
	h := newHarness(t)
	h.fake.addFault("POST", `/issues$`, fault{drop: true})
	created := h.create(keyedCreate)
	if !created.Created || h.issueCount() != 1 || h.fake.count("POST ") != 1 {
		t.Fatalf("a lost answer gave %+v with %d issues after %d creates", created, h.issueCount(), h.fake.count("POST "))
	}
	// A 5xx after the write is a lost answer too.
	h.fake.addFault("POST", `/issues$`, statusFault(502, "502 Bad Gateway", false))
	created = h.create(`{"title":"Second","idempotencyKey":"key-2"}`)
	if !created.Created || h.issueCount() != 2 {
		t.Fatalf("a 502 after the write gave %+v with %d issues", created, h.issueCount())
	}
	// A create in a closed state whose close is lost is read back.
	h.fake.addFault("PUT", `/issues/[0-9]+$`, fault{drop: true})
	created = h.create(`{"title":"Third","state":"done","idempotencyKey":"key-3"}`)
	if created.Task.State != collab.TaskStateDone {
		t.Fatalf("a lost close answered %+v", created.Task)
	}
}

func TestAnUnestablishedWriteIsUnresolvedAndSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "an-unestablished-write-is-unresolved-and-sent-once")
	h := newHarness(t)
	// The create never reached GitLab's store, and nothing shows it.
	h.fake.addFault("POST", `/issues$`, fault{drop: true, before: true})
	failure := h.fails("create", keyedCreate, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" || h.fake.count("POST ") != 1 || h.issueCount() != 0 {
		t.Fatalf("an unestablished create: %+v after %d creates", failure, h.fake.count("POST "))
	}
	// The update was lost before GitLab applied it, so reading it back
	// never shows it.
	issue := h.seed("Seeded", "")
	ref := refJSON(refOf(issue.iid))
	h.fake.addFault("PUT", `/issues/[0-9]+$`, fault{drop: true, before: true})
	failure = h.fails("update", `{"ref":`+ref+`,"title":"Mine"}`, collab.OutcomeUnresolved)
	if failure.Retryable || h.fake.count("PUT ") != 1 || h.issue(issue.iid).title != "Seeded" {
		t.Fatalf("an unestablished update: %+v after %d writes", failure, h.fake.count("PUT "))
	}
	// A refused close after a create leaves the issue open and says so.
	h.fake.addFault("PUT", `/issues/[0-9]+$`, statusFault(403, "403 Forbidden", true))
	failure = h.fails("create", `{"title":"Closed","state":"done","idempotencyKey":"key-close"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || !strings.Contains(failure.Message, "so it is open") {
		t.Fatalf("a refused close: %+v", failure)
	}
}

func TestALostUpdateResponseIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-update-response-is-read-back")
	h := newHarness(t)
	issue := h.seed("Seeded", "")
	ref := refJSON(refOf(issue.iid))
	for _, request := range []struct{ operation, arguments string }{
		{"update", `{"ref":` + ref + `,"title":"Renamed","labels":["x"]}`},
		{"transition", `{"ref":` + ref + `,"state":"done"}`},
		{"assign", `{"ref":` + ref + `,"assignees":["alice"]}`},
	} {
		h.fake.addFault("PUT", `/issues/[0-9]+$`, fault{drop: true})
		var result collab.TaskResult
		h.ok(request.operation, request.arguments, &result)
	}
	stored := h.issue(issue.iid)
	if stored.title != "Renamed" || stored.state != "closed" || !slices.Equal(stored.assignees, []int64{9}) || h.fake.count("PUT ") != 3 {
		t.Fatalf("after lost answers the issue is %+v, after %d writes", stored, h.fake.count("PUT "))
	}
}

func TestUpdateKeepsTheMarker(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "update-keeps-the-marker")
	h := newHarness(t)
	created := h.create(keyedCreate)
	ref := refJSON(created.Task.Ref)
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+ref+`,"body":"New body"}`, &updated)
	stored := h.issue(mustIID(t, created.Task.Ref))
	if updated.Task.Body != "New body" || !strings.HasPrefix(stored.description, "New body\n\n<!-- putnami-idempotency k=") {
		t.Fatalf("the update dropped the marker: %q", stored.description)
	}
	// The key still answers with the issue after the update.
	repeat := h.create(keyedCreate)
	if repeat.Created || repeat.Task.Ref != created.Task.Ref {
		t.Fatalf("after an update the key answered %+v", repeat)
	}
	// An update that changes nothing writes nothing.
	writes := h.fake.writes()
	h.ok("update", `{"ref":`+ref+`,"body":"New body","labels":["b","a"]}`, &updated)
	if h.fake.writes() != writes {
		t.Fatalf("an update without a change was written")
	}
}

func TestAssignReplacesTheAssigneesByUsername(t *testing.T) {
	spectest.Proves(t, feature, "informational-assign", "assign-replaces-the-assignees-by-username")
	h := newHarness(t)
	issue := h.seed("Assigned", "")
	ref := refJSON(refOf(issue.iid))
	var result collab.TaskResult
	h.ok("assign", `{"ref":`+ref+`,"assignees":["alice","bob","Alice"]}`, &result)
	if !slices.Equal(result.Task.Assignees, []string{"alice", "bob"}) || !slices.Equal(h.issue(issue.iid).assignees, []int64{9, 10}) {
		t.Fatalf("assign answered %v", result.Task.Assignees)
	}
	h.ok("assign", `{"ref":`+ref+`,"assignees":[]}`, &result)
	if len(result.Task.Assignees) != 0 || len(h.issue(issue.iid).assignees) != 0 {
		t.Fatalf("an empty list left %v", result.Task.Assignees)
	}
	// A tier without multiple assignees keeps one, and the answer says so.
	h.fake.mu.Lock()
	h.fake.singleAssignee = true
	h.fake.mu.Unlock()
	h.ok("assign", `{"ref":`+ref+`,"assignees":["bob","alice"]}`, &result)
	if !slices.Equal(result.Task.Assignees, []string{"bob"}) {
		t.Fatalf("a single-assignee tier answered %v", result.Task.Assignees)
	}
	writes := h.fake.writes()
	h.ok("assign", `{"ref":`+ref+`,"assignees":["bob"]}`, &result)
	if h.fake.writes() != writes {
		t.Fatalf("assigning the assignees an issue has was written")
	}
}

func TestMeNamesTheCredentialsAccount(t *testing.T) {
	spectest.Proves(t, feature, "informational-assign", "me-names-the-credentials-account")
	h := newHarness(t)
	issue := h.seed("Mine", "")
	var result collab.TaskResult
	h.ok("assign", `{"ref":`+refJSON(refOf(issue.iid))+`,"assignees":["@me"]}`, &result)
	if !slices.Equal(result.Task.Assignees, []string{"bot"}) {
		t.Fatalf("@me assigned %v", result.Task.Assignees)
	}
}

func TestAnUnknownUsernameIsInvalidAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "informational-assign", "an-unknown-username-is-invalid-and-writes-nothing")
	h := newHarness(t)
	issue := h.seed("Assigned", "")
	failure := h.fails("assign", `{"ref":`+refJSON(refOf(issue.iid))+`,"assignees":["alice","nobody"]}`, collab.OutcomeInvalid)
	if failure.Reason != reasonUnknownUser || !strings.Contains(failure.Message, "nobody") || h.fake.writes() != 0 {
		t.Fatalf("an unknown user: %+v after %d writes", failure, h.fake.writes())
	}
}
