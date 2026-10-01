package provider

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestAForkWithTheSameBranchIsAnotherIdentity(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-fork-with-the-same-branch-is-another-identity")
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.forks["forker:topic-a"] = strings.Repeat("e", 40)
	h.fake.mu.Unlock()
	own := h.openProposal("Own branch")
	var fork collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"forker:topic-a"},"title":"From a fork"}`, &fork)
	if !fork.Created || fork.Proposal.Ref == own.Ref || fork.Proposal.Change.Head != "forker:topic-a" ||
		fork.Proposal.Change.HeadCommit != strings.Repeat("e", 40) {
		t.Fatalf("the fork's proposal: %+v", fork)
	}
	for head, want := range map[string]collab.Ref{"topic-a": own.Ref, "forker:topic-a": fork.Proposal.Ref, "ACME:topic-a": own.Ref} {
		var found collab.ProposalListResult
		h.ok("proposals", "find", `{"change":{"base":"main","head":"`+head+`"}}`, &found)
		if len(found.Items) != 1 || found.Items[0].Ref != want || found.Items[0].Change.Head != head {
			t.Fatalf("head %s matched %+v", head, found.Items)
		}
	}
	var none collab.ProposalListResult
	h.ok("proposals", "find", `{"change":{"base":"release","head":"topic-a"}}`, &none)
	if len(none.Items) != 0 {
		t.Fatalf("another base matched %+v", none.Items)
	}
	var status collab.ProposalStatusResult
	h.ok("proposals", "status", `{"ref":`+refJSON(fork.Proposal.Ref)+`}`, &status)
	if status.Proposal.Change.Head != "forker:topic-a" || status.Proposal.Change.Repository != "acme/app" {
		t.Fatalf("status names %+v", status.Proposal.Change)
	}
}

func TestAHeadThatMovedIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-head-that-moved-is-a-conflict")
	h := newHarness(t)
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"`+strings.Repeat("f", 40)+`"},"title":"t"}`,
		collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved || h.fake.count("POST", `/pulls$`) != 0 {
		t.Fatalf("a create at another commit: %+v", failure)
	}
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"bbbbbbb"},"title":"t"}`, &created)
	if !created.Created || created.Proposal.Change.HeadCommit != strings.Repeat("b", 40) {
		t.Fatalf("an abbreviated head commit: %+v", created)
	}
	h.fake.push(number(t, created.Proposal.Ref), strings.Repeat("9", 40))
	h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"bbbbbbb"},"title":"t2"}`, collab.OutcomeConflict)
	h.fails("proposals", "upsert", `{"change":{"base":"main","head":"missing","headCommit":"bbbbbbb"},"title":"t"}`, collab.OutcomeNotFound)
	if failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"missing"},"title":"t"}`, collab.OutcomeInvalid); failure.Reason != reasonRejected {
		t.Fatalf("GitHub's refusal of an unknown head: %+v", failure)
	}
	var updated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"9999999"},"title":"t2"}`, &updated)
	if updated.Created || updated.Proposal.Revision == created.Proposal.Revision {
		t.Fatalf("an update at the new head: %+v", updated)
	}
}

func TestAPullRequestBehindItsPushedBranchIsAwaited(t *testing.T) {
	h := newHarness(t)
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"bbbbbbb"},"title":"t"}`, &created)
	n := number(t, created.Proposal.Ref)
	var waits []int
	wait := func(catchUp func(attempt int)) {
		waits = nil
		h.provider.HeadLagPause = func(_ context.Context, attempt int) error {
			waits = append(waits, attempt)
			catchUp(attempt)
			return nil
		}
	}

	// GitHub moved the branch, and the pull request follows after two reads.
	pushed := strings.Repeat("9", 40)
	wait(func(attempt int) {
		if attempt == 2 {
			h.fake.push(n, pushed)
		}
	})
	h.fake.moveBranch("topic-a", pushed)
	var updated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"9999999"},"title":"t2"}`, &updated)
	if !slices.Equal(waits, []int{1, 2}) || updated.Created || updated.Proposal.Change.HeadCommit != pushed || updated.Proposal.Title != "t2" {
		t.Fatalf("a pull request that caught up after %v waits: %+v", waits, updated)
	}

	// The pull request never catches up within the bound: retry, nothing written.
	wait(func(int) {})
	h.fake.moveBranch("topic-a", strings.Repeat("8", 40))
	patches := h.fake.count("PATCH", `.`)
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"8888888"},"title":"t3"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonHeadLagging || !failure.Retryable || len(waits) != headLagReads || h.fake.count("PATCH", `.`) != patches ||
		!strings.Contains(failure.Message, "has not caught up with the pushed branch yet") {
		t.Fatalf("a pull request that never caught up after %v waits: %+v", waits, failure)
	}

	// A branch at another commit is still a conflict, answered without waiting.
	wait(func(int) {})
	failure = h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a","headCommit":"7777777"},"title":"t3"}`, collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved || len(waits) != 0 || h.fake.count("PATCH", `.`) != patches {
		t.Fatalf("a branch at another commit after %v waits: %+v", waits, failure)
	}
}

func TestAnotherRepositoryIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "another-repository-is-refused")
	h := newHarness(t)
	failure := h.fails("proposals", "upsert", `{"change":{"repository":"acme/other","base":"main","head":"topic-a"},"title":"t"}`, collab.OutcomeInvalid)
	if failure.Reason != reasonRepository || h.fake.count("POST", `/pulls$`) != 0 {
		t.Fatalf("another repository: %+v", failure)
	}
	h.fails("proposals", "find", `{"change":{"repository":"acme/other","base":"main","head":"topic-a"}}`, collab.OutcomeInvalid)
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"repository":"ACME/App","base":"main","head":"topic-a"},"title":"t"}`, &created)
	if created.Proposal.Change.Repository != "ACME/App" || created.Proposal.Ref.Source != "github:acme/app" {
		t.Fatalf("the request's spelling is echoed and the source is canonical: %+v", created.Proposal)
	}
}

func TestUpsertEditsAndTogglesDraft(t *testing.T) {
	h := newHarness(t)
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Draft","draft":true}`, &created)
	if created.Proposal.State != collab.ProposalStateDraft {
		t.Fatalf("created %+v", created.Proposal)
	}
	var ready collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Ready","body":"now","draft":false,"expectedRevision":"`+
		created.Proposal.Revision+`"}`, &ready)
	if ready.Created || ready.Proposal.State != collab.ProposalStateOpen || ready.Proposal.Title != "Ready" || ready.Proposal.Body != "now" {
		t.Fatalf("ready %+v", ready.Proposal)
	}
	var draft collab.ProposalUpsertResult
	h.fake.fail("POST", `^/graphql$`, fault{drop: true})
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Ready","body":"now","draft":true}`, &draft)
	if draft.Proposal.State != collab.ProposalStateDraft || h.fake.count("PATCH", `/pulls/`) != 1 {
		t.Fatalf("back to draft, with the answer lost, %+v after %d patches", draft.Proposal, h.fake.count("PATCH", `/pulls/`))
	}
	h.fake.fail("PATCH", `/pulls/[0-9]+$`, fault{drop: true})
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Ready again","body":"now","draft":false}`, &ready)
	if ready.Proposal.State != collab.ProposalStateOpen || ready.Proposal.Title != "Ready again" {
		t.Fatalf("an edit whose answer was lost, then marked ready: %+v", ready.Proposal)
	}
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Ready","body":"now","draft":true}`, &draft)
	h.fake.fail("POST", `^/graphql$`, fault{before: true, status: 200, body: `{"errors":[{"type":"FORBIDDEN","message":"no"}]}`})
	if failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Ready","body":"now","draft":false}`,
		collab.OutcomeDenied); failure.Reason != reasonForbidden {
		t.Fatalf("a refused draft change: %+v", failure)
	}
	h.fake.mu.Lock()
	h.fake.issues[number(t, draft.Proposal.Ref)].state = "closed"
	h.fake.mu.Unlock()
	var reopened collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Second"}`, &reopened)
	if !reopened.Created || reopened.Proposal.Ref == draft.Proposal.Ref {
		t.Fatalf("a closed proposal is not the open one: %+v", reopened)
	}
	var all collab.ProposalListResult
	h.ok("proposals", "find", `{"change":{"base":"main","head":"topic-a"},"page":{"size":1}}`, &all)
	if len(all.Items) != 1 || all.Items[0].State != collab.ProposalStateClosed || all.Page.Next == "" {
		t.Fatalf("the first page of two: %+v", all)
	}
	h.ok("proposals", "find", `{"change":{"base":"main","head":"topic-a"},"page":{"size":1,"cursor":"`+all.Page.Next+`"}}`, &all)
	if len(all.Items) != 1 || all.Items[0].Ref != reopened.Proposal.Ref || all.Page.Next != "" {
		t.Fatalf("the second page: %+v", all)
	}
	h.ok("proposals", "find", `{"change":{"base":"main","head":"topic-a"},"states":["open"]}`, &all)
	if len(all.Items) != 1 || all.Items[0].Ref != reopened.Proposal.Ref {
		t.Fatalf("open proposals: %+v", all.Items)
	}
}

func TestUpsertAssignsAndInheritsLabelsFromClosedIssues(t *testing.T) {
	h := newHarness(t)
	h.proposals = `{"repository":"acme/app","assignAuthor":true,"inheritLabelPrefixes":["group/","scope/"]}`
	issue := h.seedIssue("Bug", "", "group/tooling", "scope/cli", "bug", "status/confirmed")
	other := h.seedIssue("Elsewhere", "", "group/other")
	body := "Fixes the thing.\n\nCloses #" + strconv.Itoa(issue.number) + "\nRefs #" + strconv.Itoa(other.number) + "\nfixes acme/else#1"
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+quoteJSON(body)+`}`, &created)
	pull := h.issue(number(t, created.Proposal.Ref))
	if !slices.Equal(pull.labels, []string{"group/tooling", "scope/cli"}) || !slices.Equal(pull.assignees, []string{"putnami-bot"}) {
		t.Fatalf("the pull request carries labels %v and assignees %v", pull.labels, pull.assignees)
	}
	labelPosts := h.fake.count("POST", `/labels$`)
	var repeated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+quoteJSON(body)+`}`, &repeated)
	if h.fake.count("POST", `/labels$`) != labelPosts || h.fake.count("POST", `/assignees$`) != 1 {
		t.Error("a repeated upsert added labels or an assignee again")
	}

	h.proposals = `{"repository":"acme/app","inheritLabelPrefixes":["group/"]}`
	h.fake.mu.Lock()
	h.fake.issues[issue.number].labels = append(h.fake.issues[issue.number].labels, "group/second")
	h.fake.mu.Unlock()
	h.fake.fail("POST", `/labels$`, statusFault(403, "Resource not accessible by integration", true))
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+quoteJSON(body)+`}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || failure.Retryable || failure.Reconcile == "" {
		t.Fatalf("a refused label after the write: %+v", failure)
	}
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+quoteJSON(body)+`}`, &repeated)
	if got := h.issue(number(t, created.Proposal.Ref)).labels; !slices.Contains(got, "group/second") {
		t.Fatalf("the repeat did not finish the labels: %v", got)
	}
}

// TestAProposalAnswerReportsItsAssigneesAndLabels: every answer about a pull
// request carries its labels and assignees, [] when it has none, so a caller
// can verify what assignAuthor and inheritLabelPrefixes applied; and a label
// or an assignee changed by anyone moves the revision.
func TestAProposalAnswerReportsItsAssigneesAndLabels(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-proposal-answer-reports-its-assignees-and-labels")
	h := newHarness(t)
	plain := h.openProposal("Plain")
	if plain.Labels == nil || plain.Assignees == nil || len(plain.Labels)+len(plain.Assignees) != 0 {
		t.Fatalf("a pull request without labels or assignees answered labels %v and assignees %v", plain.Labels, plain.Assignees)
	}

	h.proposals = `{"repository":"acme/app","assignAuthor":true,"inheritLabelPrefixes":["group/","scope/"]}`
	issue := h.seedIssue("Bug", "", "group/tooling", "scope/cli", "bug")
	body := quoteJSON("Closes #" + strconv.Itoa(issue.number))
	var upserted collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+body+`}`, &upserted)
	wantLabels, wantAssignees := []string{"group/tooling", "scope/cli"}, []string{"putnami-bot"}
	if !slices.Equal(upserted.Proposal.Labels, wantLabels) || !slices.Equal(upserted.Proposal.Assignees, wantAssignees) {
		t.Fatalf("the upsert answered labels %v and assignees %v", upserted.Proposal.Labels, upserted.Proposal.Assignees)
	}
	var status collab.ProposalStatusResult
	h.ok("proposals", "status", `{"ref":`+refJSON(upserted.Proposal.Ref)+`}`, &status)
	var found collab.ProposalListResult
	h.ok("proposals", "find", `{"change":{"base":"main","head":"topic-a"}}`, &found)
	for name, read := range map[string]collab.Proposal{"status": status.Proposal, "find": found.Items[0]} {
		if !slices.Equal(read.Labels, wantLabels) || !slices.Equal(read.Assignees, wantAssignees) || read.Revision != upserted.Proposal.Revision {
			t.Fatalf("%s read labels %v, assignees %v and revision %s after the upsert answered %s", name, read.Labels, read.Assignees,
				read.Revision, upserted.Proposal.Revision)
		}
	}

	h.fake.mu.Lock()
	h.fake.issues[number(t, upserted.Proposal.Ref)].labels = append(h.fake.issues[number(t, upserted.Proposal.Ref)].labels, "needs-review")
	h.fake.mu.Unlock()
	h.ok("proposals", "status", `{"ref":`+refJSON(upserted.Proposal.Ref)+`}`, &status)
	if status.Proposal.Revision == upserted.Proposal.Revision || !slices.Contains(status.Proposal.Labels, "needs-review") {
		t.Fatalf("a label added by someone else left revision %s and labels %v", status.Proposal.Revision, status.Proposal.Labels)
	}
	h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"Fix","body":`+body+
		`,"expectedRevision":"`+upserted.Proposal.Revision+`"}`, collab.OutcomeConflict)
}

func quoteJSON(value string) string {
	return strconv.Quote(value)
}

func TestStatusSummarizesChecksAndReviews(t *testing.T) {
	h := newHarness(t)
	proposal := h.openProposal("Checked")
	ref := refJSON(proposal.Ref)
	var status collab.ProposalStatusResult
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Checks.State != collab.ChecksStateNone || status.Checks.Commit != strings.Repeat("b", 40) || len(status.Reviews) != 0 {
		t.Fatalf("no checks: %+v", status)
	}

	sha := strings.Repeat("b", 40)
	h.fake.mu.Lock()
	h.fake.checkRuns[sha] = []map[string]any{
		{"name": "gate", "status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/app/runs/1"},
		{"name": "lint", "status": "completed", "conclusion": "skipped"},
	}
	h.fake.statuses[sha] = []map[string]any{{"context": "ci/legacy", "state": "success", "target_url": "https://user:pass@ci.example/1"}}
	h.fake.mu.Unlock()
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Checks.State != collab.ChecksStatePassing || len(status.Checks.Items) != 3 ||
		status.Checks.Items[0].Name != "ci/legacy" || status.Checks.Items[0].URL != "" || status.Checks.Items[1].URL == "" {
		t.Fatalf("passing checks: %+v", status.Checks)
	}

	h.fake.mu.Lock()
	h.fake.checkRuns[sha] = append(h.fake.checkRuns[sha], map[string]any{"name": "slow", "status": "in_progress"})
	h.fake.mu.Unlock()
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Checks.State != collab.ChecksStatePending || status.Checks.Items[0].Name != "slow" {
		t.Fatalf("pending checks: %+v", status.Checks)
	}

	h.fake.mu.Lock()
	for i := 0; i < 130; i++ {
		h.fake.checkRuns[sha] = append(h.fake.checkRuns[sha], map[string]any{"name": "matrix " + strconv.Itoa(i), "status": "completed", "conclusion": "success"})
	}
	h.fake.statuses[sha] = append(h.fake.statuses[sha], map[string]any{"context": "deploy\npreview", "state": "error"})
	h.fake.mu.Unlock()
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Checks.State != collab.ChecksStateFailing || len(status.Checks.Items) != collab.MaxListMembers ||
		status.Checks.Items[0].Name != "deploy preview" || !strings.Contains(status.Checks.Detail, "135 checks") {
		t.Fatalf("failing checks beyond the list bound: state %s, %d items, first %+v, detail %q",
			status.Checks.State, len(status.Checks.Items), status.Checks.Items[0], status.Checks.Detail)
	}

	var review collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Looks fine.","commit":"bbbbbbb","idempotencyKey":"r1"}`, &review)
	if review.Review.Commit != sha || review.Review.Author != "putnami-bot" || !strings.HasPrefix(review.Review.Ref.ID, "review-") {
		t.Fatalf("review %+v", review.Review)
	}
	h.fake.mu.Lock()
	n := number(t, proposal.Ref)
	h.fake.reviews[n] = append(h.fake.reviews[n],
		&fakeReview{id: 1, user: "octocat", state: "PENDING", commit: sha, submitted: h.fake.clock},
		&fakeReview{id: 2, user: "hubot", state: "DISMISSED", commit: sha, submitted: h.fake.clock},
		&fakeReview{id: 3, user: "hubot", state: "APPROVED", commit: sha, submitted: h.fake.clock})
	h.fake.mu.Unlock()
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if len(status.Reviews) != 2 || status.Reviews[1].Verdict != collab.ReviewVerdictApprove || status.Reviews[1].Author != "hubot" {
		t.Fatalf("reviews %+v", status.Reviews)
	}
}

func TestReviewRules(t *testing.T) {
	h := newHarness(t)
	proposal := h.openProposal("Mine")
	ref := refJSON(proposal.Ref)
	failure := h.fails("proposals", "review", `{"ref":`+ref+`,"verdict":"approve","body":"","idempotencyKey":"own"}`, collab.OutcomeInvalid)
	mustContain(t, failure.Message, "Can not approve your own pull request")
	var review collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"","idempotencyKey":"empty"}`, &review)
	if !review.Created {
		t.Fatalf("a comment with an empty body carries the marker and is accepted: %+v", review)
	}
	h.fails("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"x","commit":"0000000","idempotencyKey":"old"}`, collab.OutcomeInvalid)
}

func TestMergeIsRefusedUnlessAllowed(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "merge-is-refused-unless-allowed")
	h := newHarness(t)
	proposal := h.openProposal("Merge me")
	ref := refJSON(proposal.Ref)
	sha := strings.Repeat("b", 40)
	failure := h.fails("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"`+sha+`"}`, collab.OutcomeDenied)
	if failure.Reason != reasonMergeDisabled || h.fake.count("PUT", `/merge$`) != 0 || h.fake.count("GET", `/pulls/`) != 0 {
		t.Fatalf("merge without allowMerge: %+v", failure)
	}

	h.proposals = `{"repository":"acme/app","allowMerge":true}`
	if failure := h.fails("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"1234567"}`, collab.OutcomeConflict); failure.Reason != reasonHeadMoved ||
		h.fake.count("PUT", `/merge$`) != 0 {
		t.Fatalf("a moved head: %+v", failure)
	}
	h.fake.fail("PUT", `/merge$`, statusFault(409, "Head branch was modified. Review and try the merge again.", true))
	if failure := h.fails("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"bbbbbbb"}`, collab.OutcomeConflict); failure.Reason != reasonHeadMoved {
		t.Fatalf("GitHub's head check: %+v", failure)
	}
	h.fake.fail("PUT", `/merge$`, statusFault(405, "Pull Request is not mergeable", true))
	if failure := h.fails("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"bbbbbbb"}`, collab.OutcomeConflict); failure.Reason != reasonNotMergeable {
		t.Fatalf("not mergeable: %+v", failure)
	}
	h.fake.fail("PUT", `/merge$`, fault{drop: true})
	var merged collab.ProposalResult
	h.ok("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"bbbbbbb","method":"squash"}`, &merged)
	if merged.Proposal.State != collab.ProposalStateMerged {
		t.Fatalf("a merge whose answer was lost: %+v", merged.Proposal)
	}
	h.ok("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"bbbbbbb"}`, &merged)
	if h.fake.count("PUT", `/merge$`) != 3 {
		t.Fatalf("merging a merged proposal sent another merge: %d", h.fake.count("PUT", `/merge$`))
	}
	other := h.openProposalOn("topic-b")
	h.fake.mu.Lock()
	h.fake.issues[number(t, other.Ref)].state = "closed"
	h.fake.mu.Unlock()
	h.fails("proposals", "merge", `{"ref":`+refJSON(other.Ref)+`,"expectedHeadCommit":"ccccccc"}`, collab.OutcomeConflict)
}

func (h *harness) openProposalOn(branch string) collab.Proposal {
	h.t.Helper()
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"`+branch+`"},"title":"On `+branch+`"}`, &created)
	return created.Proposal
}
