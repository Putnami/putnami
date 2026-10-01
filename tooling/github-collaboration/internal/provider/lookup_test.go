package provider

import (
	"strconv"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// The key lookup of tasks.create reads by number what GitHub's issue lists
// may not show yet. These tests hold it to the numbers that never become
// listed issues: deleted issues (410), issues moved to another repository
// (a redirect) and discussions (404).

// seedDiscussion gives the next number to a discussion: the issues endpoint
// answers 404 for it, no list shows it, and GitHub's GraphQL API finds it.
func (h *harness) seedDiscussion() {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.discussions[h.fake.next] = true
	h.fake.next++
}

// discussionAccess sets whether the repository takes discussions and whether
// the credential can read them.
func (h *harness) discussionAccess(enabled, readable bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.discussionsOff = !enabled
	h.fake.discussionsForbidden = !readable
}

// seedGone adds an issue and deletes it, or moves it to another repository.
func (h *harness) seedGone(moved bool) {
	issue := h.seedIssue("Gone", "")
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if moved {
		issue.movedTo = "acme/elsewhere"
		return
	}
	issue.deleted = true
}

// age moves GitHub's clock forward.
func (h *harness) age(d time.Duration) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.clock = h.fake.clock.Add(d)
}

// numberReads counts the reads by number of the repository's issues.
func (h *harness) numberReads() int {
	return h.fake.count("GET", `^/repos/acme/app/issues/[0-9]+$`)
}

func TestDeletedAndMovedIssuesNeverBlockACreate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "deleted-and-moved-issues-never-block-a-create")
	h := newHarness(t)

	// Among the recent issues, 40 numbers are deleted, moved or discussions:
	// each is read, and none holds the key.
	for i := 0; i < 40; i++ {
		h.seedIssue("Kept "+strconv.Itoa(i), "")
		switch i % 3 {
		case 0:
			h.seedGone(false)
		case 1:
			h.seedGone(true)
		default:
			h.seedDiscussion()
		}
	}
	h.seedIssue("Newest", "")
	before := h.numberReads()
	among := h.createTask("Among gaps", "among-gaps")
	if reads := h.numberReads() - before; reads != 40+notGivenRun {
		t.Fatalf("the lookup read %d numbers, want the 40 gaps and %d above the newest issue", reads, notGivenRun)
	}

	// The 40 newest numbers are deleted or moved: the lookup reads past them.
	for i := 0; i < 40; i++ {
		h.seedGone(i%2 == 0)
	}
	above := h.createTask("Above gaps", "above-gaps")
	if above.Ref == among.Ref || h.fake.count("POST", `/issues$`) != 2 {
		t.Fatalf("a create above 40 gone numbers: %+v", above)
	}

	// Once the newest listed issue is older than the lag bound, no number
	// below it is read: an issue there would be listed.
	h.age(2 * listLagBound)
	before = h.numberReads()
	h.createTask("Settled", "settled")
	if reads := h.numberReads() - before; reads != notGivenRun {
		t.Fatalf("a lookup below a settled issue read %d numbers, want %d", reads, notGivenRun)
	}
}

func TestADiscussionOrAMovedIssueAboveTheNewestDoesNotHideARepeat(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-number-above-the-newest-issue-that-is-no-issue-does-not-hide-a-repeat")
	h := newHarness(t)
	h.seedIssue("Listed", "")
	h.fake.listLag = time.Hour
	h.seedDiscussion()
	h.seedGone(true)
	h.seedDiscussion()
	h.seedDiscussion()
	created := h.createTask("Behind", "behind")

	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Behind","idempotencyKey":"behind"}`, &replay)
	if replay.Created || replay.Task.Ref != created.Ref || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a repeat behind discussions and a moved issue: %+v after %d POSTs", replay, h.fake.count("POST", `/issues$`))
	}
	if h.fake.count("GET", `^/repos/acme/elsewhere/issues/[0-9]+$`) == 0 {
		t.Fatal("the read of the moved issue did not follow GitHub's redirect")
	}
}

func TestARunOfDiscussionsAboveTheNewestDoesNotHideARepeat(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-run-of-discussions-above-the-newest-issue-does-not-hide-a-repeat")
	h := newHarness(t)
	h.seedIssue("Listed", "")
	h.fake.listLag = time.Hour
	// More discussions in a row than the lookup takes as the end of the
	// numbers given, opened while GitHub does not list the issue behind them.
	for i := 0; i < 2*notGivenRun; i++ {
		h.seedDiscussion()
	}
	created := h.createTask("Behind discussions", "behind-discussions")

	queries := h.fake.count("POST", `^/graphql$`)
	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Behind discussions","idempotencyKey":"behind-discussions"}`, &replay)
	if replay.Created || replay.Task.Ref != created.Ref || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a repeat behind %d discussions: %+v after %d POSTs", 2*notGivenRun, replay, h.fake.count("POST", `/issues$`))
	}
	// GitHub is asked once for each run of numbers the issues endpoint does
	// not answer, not once per number.
	if asked := h.fake.count("POST", `^/graphql$`) - queries; asked != 2 {
		t.Fatalf("the repeat asked GitHub %d times which numbers discussions hold, want 2", asked)
	}

	// Once the issue is listed, the numbers above it hold nothing: one
	// question settles them.
	h.fake.mu.Lock()
	h.fake.listLag = 0
	for _, issue := range h.fake.issues {
		issue.listed = time.Time{}
	}
	h.fake.mu.Unlock()
	queries = h.fake.count("POST", `^/graphql$`)
	h.createTask("Settled", "settled")
	if asked := h.fake.count("POST", `^/graphql$`) - queries; asked != 1 {
		t.Fatalf("a create above the newest listed issue asked GitHub %d times, want 1", asked)
	}
}

func TestAKeyLookupThatCannotTellADiscussionCreatesNothing(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lookup-that-cannot-tell-a-discussion-from-a-number-not-given-creates-nothing")
	h := newHarness(t)
	h.seedIssue("Listed", "")

	// The credential cannot read the repository's discussions.
	h.discussionAccess(true, false)
	failure := h.fails("tasks", "create", `{"title":"Blind","idempotencyKey":"blind"}`, collab.OutcomeDenied)
	if failure.Reason != reasonKeyUnverifiable || failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose lookup cannot read discussions: %+v", failure)
	}
	mustContain(t, failure.Message, "whether #2 of acme/app is a discussion or a number GitHub has not given yet")
	mustContain(t, failure.Message, "give the credential read access to discussions")

	// GitHub answers the question with an error the lookup does not know.
	h.discussionAccess(true, true)
	h.fake.fail("POST", `^/graphql$`, fault{before: true, status: 200,
		body: `{"data":{"repository":{"hasDiscussionsEnabled":true,"n2":null,"n3":null,"n4":null}},"errors":[{"type":"INTERNAL","path":["repository","n2"],"message":"try later"}]}`})
	failure = h.fails("tasks", "create", `{"title":"Blind","idempotencyKey":"blind"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose lookup got an unknown answer: %+v", failure)
	}
	mustContain(t, failure.Message, "GitHub answered INTERNAL: try later")

	// A null hasDiscussionsEnabled does not say the repository takes no
	// discussions, so the null discussions it names settle nothing.
	h.fake.fail("POST", `^/graphql$`, fault{before: true, status: 200,
		body: `{"data":{"repository":{"hasDiscussionsEnabled":null,"n2":null,"n3":null,"n4":null}}}`})
	failure = h.fails("tasks", "create", `{"title":"Blind","idempotencyKey":"blind"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose lookup got a null discussions flag: %+v", failure)
	}
	mustContain(t, failure.Message, "GitHub named no discussion and no error for it")

	// GitHub's GraphQL API does not show the repository to the credential.
	h.fake.fail("POST", `^/graphql$`, fault{before: true, status: 200,
		body: `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'acme/app'."}]}`})
	failure = h.fails("tasks", "create", `{"title":"Blind","idempotencyKey":"blind"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose lookup cannot see the repository: %+v", failure)
	}
	mustContain(t, failure.Message, "GitHub answered NOT_FOUND: Could not resolve to a Repository")

	// GitHub does not answer the question at all.
	h.fake.fail("POST", `^/graphql$`, fault{before: true, status: 502, body: `{"message":"Server Error"}`, times: 3})
	failure = h.fails("tasks", "create", `{"title":"Blind","idempotencyKey":"blind"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || !failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose lookup got no answer: %+v", failure)
	}

	// A repository that takes no discussions needs no discussion read: the
	// credential's access to them does not matter.
	h.discussionAccess(false, false)
	if created := h.createTask("Blind", "blind"); created.Title != "Blind" || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a create in a repository without discussions: %+v", created)
	}
}

func TestAKeyBehindTooManyGoneNumbersIsUnverifiableUntilANewerIssueSettles(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lookup-that-cannot-finish-says-whether-a-retry-can-succeed")
	h := newHarness(t)
	h.seedIssue("Listed", "")
	for i := 0; i <= maxKeyReads; i++ {
		h.seedGone(i%4 == 0)
	}
	failure := h.fails("tasks", "create", `{"title":"Crowded","idempotencyKey":"crowded"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a key behind %d gone numbers: %+v", maxKeyReads+1, failure)
	}
	mustContain(t, failure.Message, "75 of the numbers read are deleted issues, 25 moved to another repository")
	mustContain(t, failure.Message, "until GitHub lists an issue or pull request newer than them and 30 minutes pass")

	// As the message says: a newer issue, once it is older than the lag
	// bound, puts the gone numbers behind it.
	h.seedIssue("Newer", "")
	h.age(2 * listLagBound)
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Crowded","idempotencyKey":"crowded"}`, &created)
	if !created.Created || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a create once a newer issue settled: %+v", created)
	}
}

func TestACreateInStateOpenCarriesNoStateLabel(t *testing.T) {
	spectest.Proves(t, feature, "configurable-state-mapping", "a-create-applies-a-state-label-only-when-the-state-needs-one")
	h := newHarness(t)
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Plain","labels":["bug"],"idempotencyKey":"plain"}`, &created)
	if labels := h.issue(number(t, created.Task.Ref)).labels; len(labels) != 1 || labels[0] != "bug" || created.Task.State != collab.TaskStateOpen {
		t.Fatalf("a create in state open carries %v and reads as %s", labels, created.Task.State)
	}
	h.ok("tasks", "create", `{"title":"Blocked","state":"blocked","idempotencyKey":"blocked"}`, &created)
	if labels := h.issue(number(t, created.Task.Ref)).labels; len(labels) != 1 || labels[0] != "status/needs-review" {
		t.Fatalf("a create in state blocked carries %v", labels)
	}
}
