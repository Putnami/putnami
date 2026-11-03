package provider

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const createPath = `^/rest/api/2/issue$`

func TestARepeatedKeyReturnsTheIssueItCreated(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-repeated-key-returns-the-issue-it-created")
	spectest.Proves(t, feature, "reconciled-writes", "a-reused-key-with-other-content-is-a-conflict")
	h := newHarness(t)
	first := h.create(`{"title":"Keyed","body":"b","labels":["x"],"idempotencyKey":"k1"}`)
	if !first.Created || first.Task.Key != "ENG-1" || first.Task.Body != "b" {
		t.Fatalf("created %+v", first)
	}
	again := h.create(`{"title":"Keyed","body":"b","labels":["x"],"idempotencyKey":"k1"}`)
	if again.Created || again.Task.Ref != first.Task.Ref {
		t.Fatalf("a repeat answered %+v", again)
	}
	failure := h.fails("create", `{"title":"Other","idempotencyKey":"k1"}`, collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Retryable {
		t.Fatalf("a reuse answered %+v", failure)
	}
	// Another binding's project, or another account's copy of the label,
	// never answers for the key.
	h.fake.mu.Lock()
	copied := h.fake.newIssue("Copy", "", h.fake.issues["10000"].labels, "557058:someone-else")
	h.fake.mu.Unlock()
	if h.create(`{"title":"Fresh","idempotencyKey":"k2"}`).Task.Key == copied.key() || h.issueCount() != 3 {
		t.Fatal("a fresh key answered with an existing issue")
	}
}

func TestARepeatBeforeTheSearchShowsTheIssueReturnsIt(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-repeat-before-the-search-shows-the-issue-returns-it")
	h := newHarness(t)
	h.fake.searchLag = time.Hour
	h.seed("Older", "")
	first := h.create(`{"title":"Lagging","idempotencyKey":"lag"}`)
	h.seed("Newer, by someone else", "")
	again := h.create(`{"title":"Lagging","idempotencyKey":"lag"}`)
	if again.Created || again.Task.Ref != first.Task.Ref || h.issueCount() != 3 {
		t.Fatalf("a repeat inside the search delay answered %+v; %d issues", again, h.issueCount())
	}
}

func TestALostCreateResponseReturnsTheIssueWithoutADuplicate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-create-response-returns-the-issue-without-a-duplicate")
	h := newHarness(t)
	h.fake.searchLag = time.Hour
	h.fake.addFault("POST", createPath, fault{drop: true})
	created := h.create(`{"title":"Lost","idempotencyKey":"lost"}`)
	if !created.Created || created.Task.Title != "Lost" || h.issueCount() != 1 || h.fake.count("POST /rest/api/2/issue") != 1 {
		t.Fatalf("a lost answer gave %+v and %d issues", created, h.issueCount())
	}
	h.fake.addFault("POST", createPath, statusFault(http.StatusBadGateway, "bad gateway", false))
	if !h.create(`{"title":"Gateway","idempotencyKey":"gw"}`).Created || h.issueCount() != 2 {
		t.Fatal("a 502 after the write opened a second issue")
	}
}

func TestAnUnestablishedWriteIsUnresolvedAndSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "an-unestablished-write-is-unresolved-and-sent-once")
	h := newHarness(t)
	h.fake.addFault("POST", createPath, fault{drop: true, before: true})
	failure := h.fails("create", `{"title":"Unknown","idempotencyKey":"unknown"}`, collab.OutcomeUnresolved)
	if failure.Retryable || !strings.Contains(failure.Reconcile, "tasks.create") || h.fake.count("POST /rest/api/2/issue") != 1 || h.issueCount() != 0 {
		t.Fatalf("an unknown create answered %+v", failure)
	}

	issue := h.seed("Edited", "")
	h.fake.addFault("PUT", `^/rest/api/2/issue/`, fault{status: http.StatusInternalServerError, before: true, body: `{}`})
	failure = h.fails("update", `{"ref":`+h.ref(issue)+`,"title":"New"}`, collab.OutcomeUnresolved)
	if failure.Retryable || !strings.Contains(failure.Reconcile, "tasks.get") || h.fake.count("PUT ") != 1 {
		t.Fatalf("an unknown update answered %+v after %d sends", failure, h.fake.count("PUT "))
	}
}

func TestALostTransitionResponseIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-transition-response-is-read-back")
	h := newHarness(t)
	issue := h.seed("Moved", "")
	h.fake.addFault("POST", `/transitions$`, fault{drop: true})
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+h.ref(issue)+`,"state":"done"}`, &moved)
	if moved.Task.State != collab.TaskStateDone || h.fake.count("POST /rest/api/2/issue/"+strconv.Itoa(issue.id)+"/transitions") != 1 {
		t.Fatalf("a lost transition answered %+v", moved.Task)
	}
}

func TestACreateThatMovesFailsNamesTheIssue(t *testing.T) {
	h := newHarness(t)
	var created collab.TaskCreateResult
	h.ok("create", `{"title":"Started","state":"in_progress","idempotencyKey":"started"}`, &created)
	if created.Task.State != collab.TaskStateInProgress {
		t.Fatalf("created %+v", created.Task)
	}
	h.fake.addFault("POST", `/transitions$`, statusFault(http.StatusBadRequest, "a validator refused", true))
	failure := h.fails("create", `{"title":"Refused","state":"done","idempotencyKey":"refused"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || !strings.Contains(failure.Message, "ENG-2") {
		t.Fatalf("a create whose transition failed answered %+v", failure)
	}
}

func TestTheIdempotencyLabelIsHiddenAndRefused(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "the-idempotency-label-is-hidden-and-refused")
	h := newHarness(t)
	created := h.create(`{"title":"Hidden","labels":["keep"],"idempotencyKey":"hidden"}`)
	if !slices.Equal(created.Task.Labels, []string{"keep"}) {
		t.Fatalf("labels %v", created.Task.Labels)
	}
	raw := h.issue(created.Task.Ref.ID).labels
	if len(raw) != 2 || !strings.HasPrefix(raw[1], reservedLabelPrefix) || len(raw[1]) != len(reservedLabelPrefix)+64 {
		t.Fatalf("stored labels %v", raw)
	}
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+refJSON(created.Task.Ref)+`,"labels":["other"]}`, &updated)
	if !slices.Equal(updated.Task.Labels, []string{"other"}) || !slices.Contains(h.issue(created.Task.Ref.ID).labels, raw[1]) {
		t.Fatalf("an update dropped the record: %v", h.issue(created.Task.Ref.ID).labels)
	}
	if again := h.create(`{"title":"Hidden","labels":["keep"],"idempotencyKey":"hidden"}`); again.Created {
		t.Fatal("an update of the labels lost the key")
	}
	for _, arguments := range []string{
		`{"title":"x","labels":["Putnami-Idempotency-1"],"idempotencyKey":"r"}`,
	} {
		if failure := h.fails("create", arguments, collab.OutcomeInvalid); failure.Reason != reasonReservedLabel {
			t.Fatalf("%+v", failure)
		}
	}
	if failure := h.fails("update", `{"ref":`+refJSON(created.Task.Ref)+`,"labels":["putnami-idempotency-x"]}`, collab.OutcomeInvalid); failure.Reason != reasonReservedLabel {
		t.Fatalf("%+v", failure)
	}
}

func TestUpdateChangesWhatItNames(t *testing.T) {
	h := newHarness(t)
	issue := h.seed("Title", "Body", "a")
	before := h.get(issue)
	var updated collab.TaskResult
	h.ok("update", `{"ref":`+h.ref(issue)+`,"body":""}`, &updated)
	if updated.Task.Body != "" || updated.Task.Title != "Title" || !slices.Equal(updated.Task.Labels, []string{"a"}) || updated.Task.Revision == before.Revision {
		t.Fatalf("updated %+v", updated.Task)
	}
	puts := h.fake.count("PUT ")
	h.ok("update", `{"ref":`+h.ref(issue)+`,"title":"Title","labels":["a"]}`, &updated)
	if h.fake.count("PUT ") != puts {
		t.Fatal("an update that changes nothing was sent")
	}
}
