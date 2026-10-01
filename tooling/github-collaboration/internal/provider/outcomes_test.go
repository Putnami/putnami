package provider

import (
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// A write whose first part is known to have happened says so: the answer
// names what exists, and a refusal every repeat would meet again is not
// presented as something a repeat finishes.

func TestACreateWhoseCloseFailsNamesTheOpenIssue(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-partly-applied-write-names-what-happened")
	h := newHarness(t)
	h.fake.fail("PATCH", `/issues/[0-9]+$`, statusFault(403, "Resource not accessible by integration", true))
	failure := h.fails("tasks", "create", `{"title":"Born done","state":"done","idempotencyKey":"born-done"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || failure.Retryable || h.fake.count("POST", `/issues$`) != 1 || h.issue(1).state != "open" {
		t.Fatalf("a create whose close was refused: %+v", failure)
	}
	mustContain(t, failure.Message, "opened #1")
	mustContain(t, failure.Message, "so it is open")
	mustContain(t, failure.Reconcile, "transition it to done")

	// A close whose answer is lost is read back.
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true})
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Born done too","state":"done","idempotencyKey":"born-done-too"}`, &created)
	if !created.Created || created.Task.State != collab.TaskStateDone || h.issue(number(t, created.Task.Ref)).state != "closed" {
		t.Fatalf("a create whose close answer was lost: %+v", created)
	}
	// A close lost before GitHub read it, and a read-back that fails, stays
	// unresolved without claiming a refusal.
	h.fake.fail("PATCH", `/issues/[0-9]+$`, fault{drop: true, before: true})
	// The lookup's three reads above the newest issue pass.
	h.fake.fail("GET", `/issues/[0-9]+$`, fault{drop: true, before: true, times: 12, after: notGivenRun})
	failure = h.fails("tasks", "create", `{"title":"Unknown","state":"canceled","idempotencyKey":"unknown-close"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonUncertain || !strings.Contains(failure.Reconcile, "transition it to canceled") {
		t.Fatalf("a create whose close outcome is unknown: %+v", failure)
	}
}

func TestAnAcknowledgedLinkIsOKWhileItsReadLags(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-partly-applied-write-names-what-happened")
	h := newHarness(t)
	child := h.createTask("Child", "child")
	parent := h.createTask("Parent", "parent")
	// The read before the write passes; every read-back answers no parent yet.
	h.fake.fail("GET", `/parent$`, fault{before: true, status: 404, body: `{"message":"Not Found"}`, times: 5, after: 1})
	var linked collab.TaskResult
	h.ok("tasks", "link", `{"ref":`+refJSON(child.Ref)+`,"parent":`+refJSON(parent.Ref)+`}`, &linked)
	if linked.Task.Parent == nil || *linked.Task.Parent != parent.Ref || h.fake.count("POST", `/sub_issues$`) != 1 {
		t.Fatalf("an acknowledged link whose read lags: %+v", linked.Task)
	}
}

func TestARefusedAdditionSaysEveryRepeatIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-partly-applied-write-names-what-happened")
	h := newHarness(t)
	h.proposals = `{"repository":"acme/app","assignAuthor":true}`
	h.fake.fail("POST", `/assignees$`, statusFault(403, "Resource not accessible by integration", true))
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":"b"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || failure.Retryable {
		t.Fatalf("a refused assignment after the write: %+v", failure)
	}
	mustContain(t, failure.Message, "as it will be on every repeat")
	mustContain(t, failure.Reconcile, "settings.assignAuthor")

	// A failure that is not a refusal is finished by the repeat.
	h.fake.fail("POST", `/assignees$`, fault{drop: true, before: true})
	h.fake.mu.Lock()
	for _, issue := range h.fake.issues {
		issue.assignees = nil
	}
	h.fake.mu.Unlock()
	failure = h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":"b"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || strings.Contains(failure.Message, "every repeat") {
		t.Fatalf("an assignment whose answer was lost: %+v", failure)
	}
	mustContain(t, failure.Reconcile, "applies the rest")
}

func TestAnUpsertBeforeGitHubListsThePullRequestNeverOpensASecond(t *testing.T) {
	spectest.Proves(t, feature, "reconciled-writes", "a-lost-upsert-response-returns-the-pull-request-without-a-duplicate")
	h := newHarness(t)
	// GitHub's pull request list trails a create; a read by number does not.
	h.fake.pullListLag = time.Hour
	h.fake.fail("POST", `/pulls$`, fault{drop: true})
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Lagging","body":"b"}`, collab.OutcomeUnresolved)
	if failure.Retryable || h.fake.count("POST", `/pulls$`) != 1 {
		t.Fatalf("a lost upsert answer while the list lags: %+v", failure)
	}
	mustContain(t, failure.Reconcile, "proposals.find")
	// The repeat meets GitHub's one-open-pull-request rule: still unresolved
	// while the list lags, never a second pull request.
	h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Lagging","body":"b"}`, collab.OutcomeUnresolved)
	pulls := 0
	h.fake.mu.Lock()
	for _, pull := range h.fake.pulls {
		pulls++
		pull.listed = time.Time{}
	}
	h.fake.mu.Unlock()
	if pulls != 1 {
		t.Fatalf("%d pull requests after two upserts of one change", pulls)
	}
	var found collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Lagging","body":"b"}`, &found)
	if found.Created || found.Proposal.Ref.ID != strconv.Itoa(1) {
		t.Fatalf("the upsert once GitHub lists the pull request: %+v", found)
	}
}
