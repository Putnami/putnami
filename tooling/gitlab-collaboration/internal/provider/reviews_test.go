package provider

import (
	"strconv"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// reviewed is a harness with one merge request another account opened, so
// the credential's account may approve it.
func reviewed(t *testing.T) (*harness, collab.Ref) {
	h := newProposals(t, "")
	mr := h.seedMR("topic", "main", "Someone's change", otherID, h.fake.projectID)
	return h, mrRef(mr.iid)
}

func (h *harness) review(ref collab.Ref, verdict, body, key, extra string) collab.ProposalReviewResult {
	h.t.Helper()
	var result collab.ProposalReviewResult
	h.ok("review", `{"ref":`+refJSON(ref)+`,"verdict":"`+verdict+`","body":`+strconv.Quote(body)+`,"idempotencyKey":"`+key+`"`+extra+`}`, &result)
	return result
}

func (h *harness) status(ref collab.Ref) collab.ProposalStatusResult {
	h.t.Helper()
	var result collab.ProposalStatusResult
	h.ok("status", `{"ref":`+refJSON(ref)+`}`, &result)
	return result
}

func (h *harness) approvedBy(ref collab.Ref) []int64 {
	h.t.Helper()
	var approvers []int64
	h.mr(mrIID(h.t, ref), func(mr *fakeMR) { approvers = append(approvers, mr.approvedBy...) })
	return approvers
}

func (h *harness) noteCount(ref collab.Ref) int {
	h.t.Helper()
	count := 0
	h.mr(mrIID(h.t, ref), func(mr *fakeMR) { count = len(mr.notes) })
	return count
}

func TestARepeatedReviewKeyReturnsItsNote(t *testing.T) {
	spectest.Proves(t, feature, "note-reviews", "a-repeated-review-key-returns-its-note")
	h, ref := reviewed(t)
	first := h.review(ref, "comment", "Looks fine.", "key-a", "")
	if !first.Created || first.Review.Verdict != collab.ReviewVerdictComment || first.Review.Author != "bot" ||
		first.Review.Commit != fakeSHA(2) || first.Review.Ref.Source != source {
		t.Fatalf("published %+v", first)
	}
	repeat := h.review(ref, "comment", "Looks fine.", "key-a", "")
	if repeat.Created || repeat.Review.Ref != first.Review.Ref || h.noteCount(ref) != 1 {
		t.Fatalf("a repeat answered %+v with %d notes", repeat, h.noteCount(ref))
	}
	failure := h.fails("review", `{"ref":`+refJSON(ref)+`,"verdict":"comment","body":"Other.","idempotencyKey":"key-a"}`, collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch {
		t.Fatalf("a reused key: %+v", failure)
	}
	// A note another account wrote never answers for a key, even a copy.
	var body string
	h.mr(mrIID(t, ref), func(mr *fakeMR) { body = mr.notes[0].body })
	h.mr(mrIID(t, ref), func(mr *fakeMR) { h.fake.addNote(mr, body, otherID) })
	h.mr(mrIID(t, ref), func(mr *fakeMR) { mr.notes = mr.notes[1:] })
	again := h.review(ref, "comment", "Looks fine.", "key-a", "")
	if !again.Created || h.noteCount(ref) != 2 {
		t.Fatalf("a copied marker answered for the key: %+v", again)
	}
	h.fails("review", `{"ref":`+refJSON(ref)+`,"verdict":"comment","body":"x","commit":"not-a-sha","idempotencyKey":"key-b"}`, collab.OutcomeInvalid)
}

func TestALostReviewAnswerIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "note-reviews", "a-lost-review-answer-is-read-back")
	h, ref := reviewed(t)
	h.fake.addFault("POST", `/notes$`, fault{drop: true})
	published := h.review(ref, "comment", "Lost answer.", "key-lost", "")
	if !published.Created || h.noteCount(ref) != 1 || h.fake.count("POST ") != 1 {
		t.Fatalf("a lost note answered %+v with %d notes", published, h.noteCount(ref))
	}
	h.fake.addFault("POST", `/notes$`, fault{status: 502, body: `{"message":"bad gateway"}`, before: true})
	failure := h.fails("review", `{"ref":`+refJSON(ref)+`,"verdict":"comment","body":"Unknown.","idempotencyKey":"key-unknown"}`, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" || h.noteCount(ref) != 1 {
		t.Fatalf("an unestablished note: %+v", failure)
	}
}

func TestApproveApprovesTheReviewedHead(t *testing.T) {
	spectest.Proves(t, feature, "note-reviews", "approve-approves-the-reviewed-head")
	h, ref := reviewed(t)
	failure := h.fails("review", `{"ref":`+refJSON(ref)+`,"verdict":"approve","body":"Ship it.","commit":"`+fakeSHA(9)+`","idempotencyKey":"stale"}`, collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved || h.fake.writes() != 0 {
		t.Fatalf("an approval of another head: %+v", failure)
	}
	approved := h.review(ref, "approve", "Ship it.", "approve-1", `,"commit":"`+fakeSHA(2)[:10]+`"`)
	if !approved.Created || approved.Review.Verdict != collab.ReviewVerdictApprove || approved.Review.Commit != fakeSHA(2) {
		t.Fatalf("approved %+v", approved)
	}
	if approvers := h.approvedBy(ref); len(approvers) != 1 || approvers[0] != botID {
		t.Fatalf("approvers %v", approvers)
	}
	h.review(ref, "approve", "Ship it.", "approve-1", `,"commit":"`+fakeSHA(2)[:10]+`"`)
	h.review(ref, "approve", "Still fine.", "approve-2", "")
	if posts := h.fake.count("POST /api/v4/projects/acme/platform/app/merge_requests/1/approve"); posts != 1 {
		t.Fatalf("the approval was sent %d times", posts)
	}
	// GitLab refuses an approval by the merge request's author.
	own := h.upsert(`{"change":{"base":"release","head":"topic"},"title":"Mine"}`)
	failure = h.fails("review", `{"ref":`+refJSON(own.Proposal.Ref)+`,"verdict":"approve","body":"Self.","idempotencyKey":"self"}`, collab.OutcomeDenied)
	if h.noteCount(own.Proposal.Ref) != 0 {
		t.Fatalf("a refused approval posted its note: %+v", failure)
	}
	// A note that fails after the approval names what is in place.
	h.set(func(f *fakeGitLab) { f.authorMayApprove = true })
	h.fake.addFault("POST", `/notes$`, statusFault(403, "403 Forbidden", true))
	failure = h.fails("review", `{"ref":`+refJSON(own.Proposal.Ref)+`,"verdict":"approve","body":"Self.","idempotencyKey":"self-2"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete {
		t.Fatalf("a note refused after the approval: %+v", failure)
	}
	mustContain(t, failure.Message, "approved merge request !")
	retry := h.review(own.Proposal.Ref, "approve", "Self.", "self-2", "")
	if !retry.Created || len(h.approvedBy(own.Proposal.Ref)) != 1 {
		t.Fatalf("the repeat answered %+v", retry)
	}
}

func TestRequestChangesWithdrawsTheApproval(t *testing.T) {
	spectest.Proves(t, feature, "note-reviews", "request-changes-withdraws-the-approval")
	h, ref := reviewed(t)
	h.review(ref, "approve", "Fine.", "approve", "")
	changes := h.review(ref, "request_changes", "Not yet.", "changes", "")
	if changes.Review.Verdict != collab.ReviewVerdictRequestChanges || len(h.approvedBy(ref)) != 0 {
		t.Fatalf("request_changes answered %+v, approvers %v", changes, h.approvedBy(ref))
	}
	// Without an approval, request_changes posts its note alone.
	h.review(ref, "request_changes", "Still not.", "changes-2", "")
	if unapprovals := h.fake.count("POST /api/v4/projects/acme/platform/app/merge_requests/1/unapprove"); unapprovals != 1 {
		t.Fatalf("unapprove was sent %d times", unapprovals)
	}
}

func TestStatusListsReviewNotesAndApprovals(t *testing.T) {
	spectest.Proves(t, feature, "note-reviews", "status-lists-review-notes-and-approvals")
	h, ref := reviewed(t)
	comment := h.review(ref, "comment", "First.", "one", "")
	h.mr(mrIID(t, ref), func(mr *fakeMR) {
		h.fake.addNote(mr, "An ordinary note", 9)
		mr.approvedBy = append(mr.approvedBy, 10)
	})
	status := h.status(ref)
	if len(status.Reviews) != 2 || status.Reviews[0].Ref != comment.Review.Ref ||
		status.Reviews[1].Ref.ID != "approval-1-10" || status.Reviews[1].Verdict != collab.ReviewVerdictApprove || status.Reviews[1].Author != "bob" {
		t.Fatalf("reviews %+v", status.Reviews)
	}
	if status.Proposal.Ref != ref || status.Proposal.Change.Head != "topic" || status.Proposal.Change.Repository != fakeProject {
		t.Fatalf("status of %+v", status.Proposal)
	}
}

func (h *harness) statuses(sha string, statuses ...map[string]any) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	h.fake.statuses[sha] = nil
	for _, status := range statuses {
		h.fake.statuses[sha] = append(h.fake.statuses[sha], status)
	}
}

func TestStatusReportsTheHeadCommitStatuses(t *testing.T) {
	spectest.Proves(t, feature, "pipeline-checks", "status-reports-the-head-commit-statuses")
	h, ref := reviewed(t)
	if checks := h.status(ref).Checks; checks.State != collab.ChecksStateNone || checks.Commit != fakeSHA(2) {
		t.Fatalf("no status: %+v", checks)
	}
	h.statuses(fakeSHA(2),
		map[string]any{"name": "Putnami CI", "status": "success", "target_url": "https://ci.example.com/1"},
		map[string]any{"name": "lint", "status": "running"})
	checks := h.status(ref).Checks
	if checks.State != collab.ChecksStatePending || len(checks.Items) != 2 || checks.Items[0].Name != "lint" ||
		checks.Items[1].Name != "Putnami CI" || checks.Items[1].State != collab.CheckStatePassing || checks.Items[1].URL != "https://ci.example.com/1" {
		t.Fatalf("checks %+v", checks)
	}
	h.statuses(fakeSHA(2), map[string]any{"name": "Putnami CI", "status": "success"}, map[string]any{"name": "lint", "status": "failed"})
	if state := h.status(ref).Checks.State; state != collab.ChecksStateFailing {
		t.Fatalf("a failed job: %s", state)
	}
	h.statuses(fakeSHA(2), map[string]any{"name": "Putnami CI", "status": "canceled"})
	if state := h.status(ref).Checks.State; state != collab.ChecksStateFailing {
		t.Fatalf("a canceled job: %s", state)
	}
	h.statuses(fakeSHA(2), map[string]any{"name": "Putnami CI", "status": "success"}, map[string]any{"name": "lint", "status": "success"})
	if state := h.status(ref).Checks.State; state != collab.ChecksStatePassing {
		t.Fatalf("every job passed: %s", state)
	}
}

func TestAnAllowedFailureDoesNotFailTheSummary(t *testing.T) {
	spectest.Proves(t, feature, "pipeline-checks", "an-allowed-failure-does-not-fail-the-summary")
	h, ref := reviewed(t)
	h.statuses(fakeSHA(2),
		map[string]any{"name": "Putnami CI", "status": "success"},
		map[string]any{"name": "optional", "status": "failed", "allow_failure": true},
		map[string]any{"name": "deploy", "status": "manual", "allow_failure": true},
		map[string]any{"name": "docs", "status": "skipped"})
	checks := h.status(ref).Checks
	if checks.State != collab.ChecksStatePassing {
		t.Fatalf("checks %+v", checks)
	}
	for _, check := range checks.Items {
		if check.Name != "Putnami CI" && check.State != collab.CheckStateSkipped {
			t.Fatalf("%s is %s", check.Name, check.State)
		}
	}
}

// comments reads every comment of a merge request, one per page.
func (h *harness) comments(ref collab.Ref) []collab.ProposalComment {
	h.t.Helper()
	var all []collab.ProposalComment
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		page := `{"size":1}`
		if cursor != "" {
			page = `{"size":1,"cursor":"` + cursor + `"}`
		}
		var result collab.ProposalCommentListResult
		h.ok("comments", `{"ref":`+refJSON(ref)+`,"page":`+page+`}`, &result)
		all = append(all, result.Items...)
		if cursor = result.Page.Next; cursor == "" {
			return all
		}
	}
	h.t.Fatal("the comments did not end")
	return nil
}

func TestCommentsListEveryNoteOnce(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "comments-list-every-note-once")
	h, ref := reviewed(t)
	review := h.review(ref, "request_changes", "Fix the parser.", "review", "")
	h.mr(mrIID(t, ref), func(mr *fakeMR) {
		h.fake.addNote(mr, "Second", 9)
		h.fake.addNote(mr, "Third", 10)
	})
	all := h.comments(ref)
	if len(all) != 3 || all[0].Body != "Fix the parser." || all[0].Review == nil || *all[0].Review != review.Review.Ref ||
		all[1].Body != "Second" || all[1].Author != "alice" || all[1].Review != nil || all[2].Body != "Third" {
		t.Fatalf("comments %+v", all)
	}
	h.fails("comments", `{"ref":`+refJSON(ref)+`,"page":{"cursor":"m3"}}`, collab.OutcomeInvalid)
	h.fails("comments", `{"ref":`+refJSON(refOf(1))+`}`, collab.OutcomeNotFound)
}

func TestADiffNoteNamesItsFileLine(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "a-diff-note-names-its-file-line")
	h, ref := reviewed(t)
	line := 12
	h.mr(mrIID(t, ref), func(mr *fakeMR) {
		h.fake.addNote(mr, "On a line", 9).position = map[string]any{"head_sha": fakeSHA(2), "old_path": "a.go", "new_path": "a.go",
			"position_type": "text", "new_line": line, "old_line": nil}
		h.fake.addNote(mr, "On a deleted line", 9).position = map[string]any{"head_sha": fakeSHA(2), "old_path": "gone.go", "new_path": "",
			"position_type": "text", "new_line": nil, "old_line": 4}
		h.fake.addNote(mr, "On the file", 9).position = map[string]any{"head_sha": fakeSHA(2), "old_path": "b.go", "new_path": "b.go",
			"position_type": "file"}
	})
	all := h.comments(ref)
	if len(all) != 3 || all[0].Path != "a.go" || all[0].Line == nil || *all[0].Line != line || all[0].Commit != fakeSHA(2) {
		t.Fatalf("a line comment: %+v", all)
	}
	if all[1].Path != "gone.go" || all[1].Line != nil || all[2].Path != "b.go" || all[2].Line != nil {
		t.Fatalf("comments without a line of the new file: %+v %+v", all[1], all[2])
	}
}

func TestSystemNotesAreNotComments(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "system-notes-are-not-comments")
	h, ref := reviewed(t)
	h.mr(mrIID(t, ref), func(mr *fakeMR) {
		h.fake.addNote(mr, "added 1 commit", 9).system = true
		h.fake.addNote(mr, "   ", 9)
		h.fake.addNote(mr, "Said", 9)
	})
	if all := h.comments(ref); len(all) != 1 || all[0].Body != "Said" {
		t.Fatalf("comments %+v", all)
	}
}

func TestTheLatestStatusOfEachNameDecides(t *testing.T) {
	spectest.Proves(t, feature, "pipeline-checks", "the-latest-status-of-each-name-decides")
	h, ref := reviewed(t)
	h.statuses(fakeSHA(2),
		map[string]any{"id": 2, "name": "Putnami CI", "status": "success"},
		map[string]any{"id": 1, "name": "Putnami CI", "status": "failed"})
	checks := h.status(ref).Checks
	if checks.State != collab.ChecksStatePassing || len(checks.Items) != 1 {
		t.Fatalf("a failure an earlier pipeline reported decided: %+v", checks)
	}
	h.statuses(fakeSHA(2),
		map[string]any{"id": 3, "name": "Putnami CI", "status": "success"},
		map[string]any{"id": 4, "name": "Putnami CI", "status": "failed"})
	if state := h.status(ref).Checks.State; state != collab.ChecksStateFailing {
		t.Fatalf("the latest failure: %s", state)
	}
	// Past the cap the oldest are not read: the newest decide.
	many := make([]map[string]any, 0, maxCheckPages*gitlabPageSize+1)
	for i := range maxCheckPages*gitlabPageSize + 1 {
		many = append(many, map[string]any{"id": i + 10, "name": "job " + strconv.Itoa(i), "status": "success"})
	}
	many[len(many)-1]["status"] = "failed"
	h.statuses(fakeSHA(2), many...)
	checks = h.status(ref).Checks
	mustContain(t, checks.Detail, "were not read")
	if checks.State != collab.ChecksStateFailing {
		t.Fatalf("the newest status was not read: %s", checks.State)
	}
}

func TestABlockingManualJobIsPending(t *testing.T) {
	spectest.Proves(t, feature, "pipeline-checks", "a-blocking-manual-job-is-pending")
	h, ref := reviewed(t)
	h.statuses(fakeSHA(2), map[string]any{"name": "Putnami CI", "status": "success"}, map[string]any{"name": "release", "status": "manual"})
	if state := h.status(ref).Checks.State; state != collab.ChecksStatePending {
		t.Fatalf("a blocking manual job: %s", state)
	}
}
