package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestALostCreateResponseReturnsTheIssueWithoutADuplicate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-create-response-returns-the-issue-without-a-duplicate")
	h := newHarness(t)
	h.fake.fail("POST", `/issues$`, fault{drop: true})
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Lost answer","idempotencyKey":"lost-1"}`, &created)
	if !created.Created || created.Task.Title != "Lost answer" || h.fake.count("POST", `/issues$`) != 1 || h.issueCount() != 1 {
		t.Fatalf("a create whose answer was lost: %+v after %d POSTs", created, h.fake.count("POST", `/issues$`))
	}
	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Lost answer","idempotencyKey":"lost-1"}`, &replay)
	if replay.Created || replay.Task.Ref != created.Task.Ref || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("the repeat: %+v", replay)
	}

	h.fake.fail("POST", `/issues$`, statusFault(502, "Server Error", false))
	h.ok("tasks", "create", `{"title":"Bad gateway","idempotencyKey":"lost-2"}`, &created)
	if !created.Created || h.fake.count("POST", `/issues$`) != 2 || h.issueCount() != 2 {
		t.Fatalf("a create answered 502 after it landed: %+v", created)
	}
}

func TestAnUnestablishedWriteIsUnresolvedAndSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "an-unestablished-write-is-unresolved-and-sent-once")
	h := newHarness(t)
	// The answer is lost, and every read-back of the issue list fails: the
	// two list reads before the create pass, the six after it are lost.
	h.fake.fail("POST", `/issues$`, fault{drop: true})
	h.fake.fail("GET", `/issues$`, fault{drop: true, before: true, times: 6, after: 2})
	failure := h.fails("tasks", "create", `{"title":"Unknown","idempotencyKey":"unknown-1"}`, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("an unestablished create: %+v after %d POSTs", failure, h.fake.count("POST", `/issues$`))
	}
	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Unknown","idempotencyKey":"unknown-1"}`, &replay)
	if replay.Created || h.fake.count("POST", `/issues$`) != 1 || h.issueCount() != 1 {
		t.Fatalf("reconciling with the same key created a duplicate: %+v, %d issues", replay, h.issueCount())
	}

	// A review whose key cannot be checked is never sent.
	proposal := h.openProposal("Reviewed")
	review := `{"ref":` + refJSON(proposal.Ref) + `,"verdict":"comment","body":"b","idempotencyKey":"rv"}`
	h.fake.fail("GET", `/reviews$`, fault{drop: true, before: true, times: 3})
	h.fails("proposals", "review", review, collab.OutcomeUnavailable)
	if h.fake.count("POST", `/reviews$`) != 0 {
		t.Fatal("a review whose key could not be checked was sent")
	}
	// A review lost before GitHub read it, whose read-back fails, is
	// unresolved; the repeat finds nothing and publishes it once.
	h.fake.fail("POST", `/reviews$`, fault{drop: true, before: true})
	h.fake.fail("GET", `/reviews$`, fault{status: 503, body: `{"message":"unavailable"}`, before: true, times: 6, after: 1})
	failure = h.fails("proposals", "review", review, collab.OutcomeUnresolved)
	if failure.Retryable || h.fake.count("POST", `/reviews$`) != 1 {
		t.Fatalf("an unestablished review: %+v", failure)
	}
	var published collab.ProposalReviewResult
	h.ok("proposals", "review", review, &published)
	if !published.Created || h.fake.count("POST", `/reviews$`) != 2 {
		t.Fatalf("the repeat after a review that never landed: %+v", published)
	}

	task := h.createTask("Moved", "moved")
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true, before: true})
	failure = h.fails("tasks", "transition", `{"ref":`+refJSON(task.Ref)+`,"state":"done"}`, collab.OutcomeUnresolved)
	if h.fake.count("PATCH", `/issues/[0-9]+$`) != 1 || h.issue(number(t, task.Ref)).state != "open" {
		t.Fatalf("a transition lost before GitHub read it: %+v", failure)
	}
	mustContain(t, failure.Reconcile, "tasks.get")

	// An issue already blocked through status/needs-design reads as blocked
	// before the write as after it: the read-back settles the transition only
	// when it shows the first label.
	designed := h.seedIssue("Designed", "", "status/needs-design")
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true, before: true})
	failure = h.fails("tasks", "transition", `{"ref":{"source":"github:acme/app","id":"`+strconv.Itoa(designed.number)+`"},"state":"blocked"}`, collab.OutcomeUnresolved)
	if failure.Retryable || !slices.Equal(h.issue(designed.number).labels, []string{"status/needs-design"}) {
		t.Fatalf("a label-level transition lost before GitHub read it: %+v, labels %v", failure, h.issue(designed.number).labels)
	}
}

func TestALostTransitionResponseIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-transition-response-is-read-back")
	h := newHarness(t)
	task := h.createTask("Read back", "read-back")
	ref := refJSON(task.Ref)
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true})
	var moved collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"in_progress"}`, &moved)
	if moved.Task.State != collab.TaskStateInProgress || h.fake.count("PATCH", `/issues/[0-9]+$`) != 1 {
		t.Fatalf("a transition whose answer was lost: %+v", moved.Task)
	}
	h.fake.fail("PATCH", `/issues/[0-9]+$`, statusFault(504, "Gateway Timeout", false))
	var updated collab.TaskResult
	h.ok("tasks", "update", `{"ref":`+ref+`,"title":"Renamed","labels":["x"]}`, &updated)
	if updated.Task.Title != "Renamed" || h.fake.count("PATCH", `/issues/[0-9]+$`) != 2 {
		t.Fatalf("an update answered 504 after it landed: %+v", updated.Task)
	}
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true})
	h.ok("tasks", "assign", `{"ref":`+ref+`,"assignees":["octocat"]}`, &updated)
	if len(updated.Task.Assignees) != 1 || updated.Task.Assignees[0] != "octocat" {
		t.Fatalf("an assignment whose answer was lost: %+v", updated.Task)
	}
	parent := h.createTask("Parent", "parent")
	h.fake.fail("POST", `/sub_issues$`, fault{drop: true})
	h.ok("tasks", "link", `{"ref":`+ref+`,"parent":`+refJSON(parent.Ref)+`}`, &updated)
	if updated.Task.Parent == nil || *updated.Task.Parent != parent.Ref {
		t.Fatalf("a link whose answer was lost: %+v", updated.Task)
	}
	h.fake.fail("DELETE", `/sub_issue$`, fault{drop: true, before: true})
	failure := h.fails("tasks", "link", `{"ref":`+ref+`,"unlink":true}`, collab.OutcomeUnresolved)
	if failure.Retryable {
		t.Fatalf("an unlink lost before GitHub read it: %+v", failure)
	}
}

func TestALostUpsertResponseReturnsThePullRequestWithoutADuplicate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-upsert-response-returns-the-pull-request-without-a-duplicate")
	h := newHarness(t)
	h.fake.fail("POST", `/pulls$`, fault{drop: true})
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Lost","body":"b"}`, &created)
	if !created.Created || h.fake.count("POST", `/pulls$`) != 1 {
		t.Fatalf("an upsert whose answer was lost: %+v", created)
	}
	var repeated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Lost","body":"b"}`, &repeated)
	if repeated.Created || repeated.Proposal.Ref != created.Proposal.Ref || h.fake.count("POST", `/pulls$`) != 1 {
		t.Fatalf("the repeat: %+v", repeated)
	}

	// Another client opens the pull request between this upsert's read and
	// its write: GitHub refuses the second, and the upsert updates the first.
	h.seedPull("topic-b", "Someone else's")
	h.fake.fail("GET", `/pulls$`, fault{before: true, status: 200, body: `[]`})
	var raced collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-b"},"title":"Mine","body":"b"}`, &raced)
	if raced.Created || raced.Proposal.Title != "Mine" || h.fake.count("POST", `/pulls$`) != 2 {
		t.Fatalf("an upsert that lost the race: %+v", raced)
	}

	h.fake.fail("PATCH", `/pulls/[0-9]+$`, fault{drop: true})
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-b"},"title":"Mine again","body":"b"}`, &raced)
	if raced.Proposal.Title != "Mine again" {
		t.Fatalf("an edit whose answer was lost: %+v", raced)
	}
	h.fake.fail("PATCH", `/pulls/[0-9]+$`, fault{drop: true, before: true})
	h.fake.fail("GET", `/pulls/[0-9]+$`, fault{drop: true, before: true, times: 6})
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-b"},"title":"Unknown","body":"b"}`, collab.OutcomeUnresolved)
	mustContain(t, failure.Reconcile, "proposals.upsert")
}

func (h *harness) seedPull(branch, title string) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	issue := h.fake.newIssue(title, "", nil)
	issue.pull = true
	issue.user = "octocat"
	h.fake.pulls[issue.number] = &fakePull{nodeID: "PR_" + strconv.Itoa(issue.number), base: "main", head: branch, headOwner: h.fake.owner,
		sha: h.fake.branches[branch], shas: []string{h.fake.branches[branch]}}
}

func TestALostReviewResponseReturnsTheReviewWithoutADuplicate(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-review-response-returns-the-review-without-a-duplicate")
	h := newHarness(t)
	proposal := h.openProposal("Reviewed")
	ref := refJSON(proposal.Ref)
	h.fake.fail("POST", `/reviews$`, fault{drop: true})
	var review collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Once.","idempotencyKey":"once"}`, &review)
	if !review.Created || h.fake.count("POST", `/reviews$`) != 1 {
		t.Fatalf("a review whose answer was lost: %+v", review)
	}
	var replay collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Once.","idempotencyKey":"once"}`, &replay)
	if replay.Created || replay.Review.Ref != review.Review.Ref || h.fake.count("POST", `/reviews$`) != 1 {
		t.Fatalf("the repeat: %+v", replay)
	}
	h.fake.mu.Lock()
	count := len(h.fake.reviews[number(t, proposal.Ref)])
	h.fake.mu.Unlock()
	if count != 1 {
		t.Fatalf("%d reviews", count)
	}
}

func TestAMarkerCopiedByAnotherAccountAnswersForNothing(t *testing.T) {
	h := newHarness(t)
	marker := newIdempotency("tasks.create", "copied", createRequest{Title: "Copied", State: collab.TaskStateOpen})
	planted := h.seedIssue("Planted", withMarker("", marker))
	h.fake.mu.Lock()
	planted.user = "octocat"
	h.fake.mu.Unlock()
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Copied","idempotencyKey":"copied"}`, &created)
	if !created.Created || created.Task.Ref.ID == strconv.Itoa(planted.number) {
		t.Fatalf("another account's issue answered for the key: %+v", created)
	}

	proposal := h.openProposal("Reviewed")
	review := newIdempotency("proposals.review", "copied-review", reviewRequest{Verdict: collab.ReviewVerdictComment, Body: "b"})
	h.fake.mu.Lock()
	n := number(t, proposal.Ref)
	h.fake.reviews[n] = append(h.fake.reviews[n], &fakeReview{id: 9, user: "hubot", body: withMarker("b", review), state: "COMMENTED",
		commit: h.fake.pulls[n].sha, submitted: h.fake.clock})
	h.fake.mu.Unlock()
	var published collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+refJSON(proposal.Ref)+`,"verdict":"comment","body":"b","idempotencyKey":"copied-review"}`, &published)
	if !published.Created || published.Review.Ref.ID == "review-9" {
		t.Fatalf("another account's review answered for the key: %+v", published)
	}
}

func TestAKeyOlderThanTheScanIsFoundInTheSearchIndex(t *testing.T) {
	h := newHarness(t)
	h.tasks = `{"repository":"acme/app","keyScanPages":1}`
	first := h.createTask("Old", "old-key")
	for i := 0; i < 120; i++ {
		h.seedIssue("Newer "+strconv.Itoa(i), "")
	}
	var replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Old","idempotencyKey":"old-key"}`, &replay)
	if replay.Created || replay.Task.Ref != first.Ref || h.fake.count("GET", `^/search/issues$`) == 0 {
		t.Fatalf("a key beyond the scanned pages: %+v", replay)
	}
	h.fake.fail("GET", `^/search/issues$`, fault{before: true, status: 403, body: `{"message":"API rate limit exceeded"}`,
		header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1790000000"}})
	failure := h.fails("tasks", "create", `{"title":"New","idempotencyKey":"new-key"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || !failure.Retryable || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a create whose key could not be checked: %+v", failure)
	}
	mustContain(t, failure.Message, "resets at")
}

func TestARepeatBeforeGitHubListsTheIssueReturnsIt(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-repeat-before-github-lists-the-issue-returns-it")
	h := newHarness(t)
	// GitHub's issue lists and search show a new issue seconds after its
	// create is answered; here they show none for the length of the test.
	h.fake.listLag = time.Hour
	posts := func() int { return h.fake.count("POST", `/issues$`) }
	replay := func(title, key string, want collab.Ref) {
		t.Helper()
		var again collab.TaskCreateResult
		before := posts()
		h.ok("tasks", "create", `{"title":"`+title+`","idempotencyKey":"`+key+`"}`, &again)
		if again.Created || again.Task.Ref != want || posts() != before {
			t.Fatalf("repeating key %s before GitHub listed #%s: %+v after %d new POSTs", key, want.ID, again, posts()-before)
		}
	}

	unlisted := h.createTask("Unlisted", "unlisted")
	replay("Unlisted", "unlisted", unlisted.Ref)
	if failure := h.fails("tasks", "create", `{"title":"Other","idempotencyKey":"unlisted"}`, collab.OutcomeConflict); failure.Reason != collab.ReasonIdempotencyMismatch {
		t.Fatalf("a reused key with other content: %+v", failure)
	}

	// The answer to a create is lost: the read-back finds the issue by number.
	h.fake.fail("POST", `/issues$`, fault{drop: true})
	var lost collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Lost","idempotencyKey":"lost"}`, &lost)
	if !lost.Created || lost.Task.Title != "Lost" || posts() != 2 || h.issueCount() != 2 {
		t.Fatalf("a create whose answer was lost before GitHub listed it: %+v after %d POSTs", lost, posts())
	}

	// The lists show a newer issue before older ones, and a deleted issue
	// sits among the newest numbers: every number they skip is read.
	shown := h.seedIssue("Shown", "")
	above := h.createTask("Above", "above")
	deleted := h.seedIssue("Deleted", "")
	beyond := h.createTask("Beyond", "beyond")
	h.fake.mu.Lock()
	shown.listed = time.Time{}
	deleted.deleted = true
	h.fake.mu.Unlock()
	replay("Beyond", "beyond", beyond.Ref)
	replay("Above", "above", above.Ref)
	replay("Lost", "lost", lost.Task.Ref)
	replay("Unlisted", "unlisted", unlisted.Ref)
	if h.issueCount() != 6 || posts() != 4 {
		t.Fatalf("%d issues after %d POSTs, want 6 after 4", h.issueCount(), posts())
	}
}

func TestAKeyAmongTooManyUnlistedIssuesIsUnverifiable(t *testing.T) {
	h := newHarness(t)
	h.fake.listLag = time.Hour
	for i := 0; i < maxKeyReads; i++ {
		h.seedIssue("Unlisted "+strconv.Itoa(i), "")
	}
	failure := h.fails("tasks", "create", `{"title":"Crowded","idempotencyKey":"crowded"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonKeyUnverifiable || !failure.Retryable || h.fake.count("POST", `/issues$`) != 0 {
		t.Fatalf("a create whose key could not be checked: %+v", failure)
	}
	mustContain(t, failure.Message, "100 of the numbers read are issues or pull requests GitHub does not list yet")
	h.fake.mu.Lock()
	for _, issue := range h.fake.issues {
		issue.listed = time.Time{}
	}
	h.fake.mu.Unlock()
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Crowded","idempotencyKey":"crowded"}`, &created)
	if !created.Created || h.fake.count("POST", `/issues$`) != 1 {
		t.Fatalf("a create once GitHub listed the issues: %+v", created)
	}
}

func TestAnOutageIsUnavailableAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "an-outage-is-unavailable-and-writes-nothing")
	h := newHarness(t)
	task := h.createTask("Before the outage", "before")
	posts := h.fake.count("POST", `.`)
	reads := h.fake.count("GET", `/issues/[0-9]+$`)
	h.fake.fail("GET", `.`, fault{before: true, status: 503, body: `{"message":"Service Unavailable"}`, times: 3})
	failure := h.fails("tasks", "get", `{"ref":`+refJSON(task.Ref)+`}`, collab.OutcomeUnavailable)
	if !failure.Retryable || failure.Reason != reasonUnavailable || h.fake.count("GET", `/issues/[0-9]+$`)-reads != 3 {
		t.Fatalf("a read during an outage: %+v after %d reads", failure, h.fake.count("GET", `/issues/[0-9]+$`)-reads)
	}
	h.fake.fail("GET", `.`, fault{before: true, status: 503, body: `{"message":"Service Unavailable"}`, times: 2})
	var got collab.TaskResult
	h.ok("tasks", "get", `{"ref":`+refJSON(task.Ref)+`}`, &got)

	h.fake.server.Close()
	failure = h.fails("tasks", "create", `{"title":"During the outage","idempotencyKey":"during"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonUnreachable || !failure.Retryable {
		t.Fatalf("a create while GitHub is unreachable: %+v", failure)
	}
	h.fails("tasks", "find", `{}`, collab.OutcomeUnavailable)
	h.fails("proposals", "status", `{"ref":{"source":"github:acme/app","id":"1"}}`, collab.OutcomeUnavailable)
	if h.fake.count("POST", `.`) != posts {
		t.Fatal("a write was sent during the outage")
	}
}

func TestARateLimitIsRetryableAndNeverRepeated(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-rate-limit-is-retryable-and-never-repeated")
	h := newHarness(t)
	h.fake.fail("GET", `/issues$`, fault{before: true, status: 429, body: `{"message":"You have exceeded a secondary rate limit"}`, header: map[string]string{"Retry-After": "60"}})
	failure := h.fails("tasks", "find", `{}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonRateLimited || !failure.Retryable || h.fake.count("GET", `/issues$`) != 1 {
		t.Fatalf("a rate-limited read: %+v after %d reads", failure, h.fake.count("GET", `/issues$`))
	}
	h.fake.fail("GET", `/issues/[0-9]+$`, fault{before: true, status: 403, body: `{"message":"Resource not accessible by integration"}`})
	if failure := h.fails("tasks", "get", `{"ref":{"source":"github:acme/app","id":"1"}}`, collab.OutcomeDenied); failure.Reason != reasonForbidden {
		t.Fatalf("a permission refusal: %+v", failure)
	}
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{before: true, status: 409, body: `{"message":"conflict"}`})
	task := h.createTask("Conflicted", "conflicted")
	if failure := h.fails("tasks", "update", `{"ref":`+refJSON(task.Ref)+`,"title":"x"}`, collab.OutcomeConflict); failure.Reason != reasonGitHubConflict {
		t.Fatalf("GitHub's conflict: %+v", failure)
	}
	h.env["GH_TOKEN"] = "wrong-token-value"
	h.provider = &Provider{Getenv: h.provider.Getenv, Client: h.provider.Client, ReconcilePause: noPause}
	if failure := h.fails("tasks", "find", `{}`, collab.OutcomeDenied); failure.Reason != reasonUnauthorized {
		t.Fatalf("refused credentials: %+v", failure)
	}
	if strings.Contains(h.serve("tasks", "find", `{}`), "wrong-token-value") {
		t.Fatal("a refused credential reached the answer")
	}
}
