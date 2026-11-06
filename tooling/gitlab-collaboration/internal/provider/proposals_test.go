package provider

import (
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const topicChange = `{"base":"main","head":"topic"}`

// newProposals is a harness on the proposals contract whose branch topic is
// one commit ahead of main.
func newProposals(t *testing.T, extra string) *harness {
	h := newHarness(t).proposals(extra)
	h.branch("topic", fakeSHA(2))
	return h
}

// seedMR opens a merge request directly in the fake, as another account or
// from a fork would.
func (h *harness) seedMR(source, target, title string, author, sourceProject int64) *fakeMR {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	mr := h.fake.newMR(source, target, title, author, sourceProject)
	mr.sha = h.fake.branches[source]
	return mr
}

func mrIID(t *testing.T, ref collab.Ref) int {
	t.Helper()
	iid, err := strconv.Atoi(strings.TrimPrefix(ref.ID, "!"))
	if err != nil || !strings.HasPrefix(ref.ID, "!") || ref.Source != source {
		t.Fatalf("reference %+v is not a merge request of %s", ref, source)
	}
	return iid
}

func TestOneMergeRequestPerExactIdentity(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "one-merge-request-per-exact-identity")
	h := newProposals(t, "")
	h.branch("other", fakeSHA(5))
	// A fork's branch of the same name, a merge request into another base and
	// an issue numbered 1 are other items.
	h.seedMR("topic", "main", "From a fork", otherID, 77)
	h.seedMR("topic", "release", "Into release", otherID, h.fake.projectID)
	h.seed("An issue", "")
	created := h.upsert(`{"change":` + topicChange + `,"title":"First","body":"Body"}`)
	if !created.Created || created.Proposal.Ref != mrRef(3) || created.Proposal.Change.Repository != fakeProject ||
		created.Proposal.Change.HeadCommit != fakeSHA(2) || created.Proposal.State != collab.ProposalStateOpen {
		t.Fatalf("created %+v", created)
	}
	repeat := h.upsert(`{"change":` + topicChange + `,"title":"Again","body":"Body"}`)
	if repeat.Created || repeat.Proposal.Ref != created.Proposal.Ref || repeat.Proposal.Title != "Again" {
		t.Fatalf("a repeat answered %+v", repeat)
	}
	var found collab.ProposalListResult
	h.ok("find", `{"change":`+topicChange+`}`, &found)
	if len(found.Items) != 1 || found.Items[0].Ref != created.Proposal.Ref {
		t.Fatalf("find matched %+v", found.Items)
	}
	// A merge request closed earlier for the same identity is another
	// proposal, listed by iid, one page at a time.
	closed := h.seedMR("topic", "main", "Closed before", botID, h.fake.projectID)
	h.mr(closed.iid, func(mr *fakeMR) { mr.state = "closed" })
	var page collab.ProposalListResult
	h.ok("find", `{"change":`+topicChange+`,"page":{"size":1}}`, &page)
	if len(page.Items) != 1 || page.Items[0].Ref != created.Proposal.Ref || page.Page.Next == "" {
		t.Fatalf("first page %+v", page)
	}
	h.ok("find", `{"change":`+topicChange+`,"page":{"size":1,"cursor":"`+page.Page.Next+`"}}`, &page)
	if len(page.Items) != 1 || page.Items[0].State != collab.ProposalStateClosed || page.Page.Next != "" {
		t.Fatalf("second page %+v", page)
	}
	h.ok("find", `{"change":`+topicChange+`,"states":["closed"]}`, &found)
	if len(found.Items) != 1 || found.Items[0].Title != "Closed before" {
		t.Fatalf("a state filter matched %+v", found.Items)
	}
	h.fails("find", `{"change":`+topicChange+`,"page":{"cursor":"t1.i2"}}`, collab.OutcomeInvalid)
	h.fails("status", `{"ref":`+refJSON(refOf(1))+`}`, collab.OutcomeNotFound)
}

func TestAForkHeadOrAnotherRepositoryIsInvalid(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-fork-head-or-another-repository-is-invalid")
	h := newProposals(t, "")
	failure := h.fails("upsert", `{"change":{"base":"main","head":"someone:topic"},"title":"Fork"}`, collab.OutcomeInvalid)
	if failure.Reason != reasonHeadFork {
		t.Fatalf("a fork head: %+v", failure)
	}
	h.fails("find", `{"change":{"base":"main","head":"someone:topic"}}`, collab.OutcomeInvalid)
	failure = h.fails("upsert", `{"change":{"repository":"acme/other","base":"main","head":"topic"},"title":"Elsewhere"}`, collab.OutcomeInvalid)
	if failure.Reason != reasonRepository {
		t.Fatalf("another repository: %+v", failure)
	}
	if calls := h.fake.count(""); calls != 0 {
		t.Fatalf("refused requests sent %d calls", calls)
	}
	// The repository spelled in another case is the bound project.
	h.upsert(`{"change":{"repository":"ACME/platform/App","base":"main","head":"topic"},"title":"Same"}`)
}

func TestAHeadCommitNotOnTheBranchIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-head-commit-not-on-the-branch-is-a-conflict")
	h := newProposals(t, "")
	failure := h.fails("upsert", `{"change":{"base":"main","head":"topic","headCommit":"`+fakeSHA(9)+`"},"title":"Stale"}`, collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved || h.fake.writes() != 0 {
		t.Fatalf("a stale head commit: %+v, %d writes", failure, h.fake.writes())
	}
	failure = h.fails("upsert", `{"change":{"base":"main","head":"gone","headCommit":"`+fakeSHA(9)+`"},"title":"Gone"}`, collab.OutcomeNotFound)
	if failure.Reason != reasonHeadMissing {
		t.Fatalf("a missing branch: %+v", failure)
	}
	created := h.upsert(`{"change":{"base":"main","head":"topic","headCommit":"` + fakeSHA(2)[:12] + `"},"title":"Current"}`)
	h.fails("upsert", `{"change":{"base":"main","head":"topic","headCommit":"`+fakeSHA(9)+`"},"title":"Stale",`+
		`"expectedRevision":"`+created.Proposal.Revision+`"}`, collab.OutcomeConflict)
	if h.fake.count("PUT ") != 0 {
		t.Fatal("a conflict wrote")
	}
}

func TestALaggingHeadIsAwaitedThenRetryable(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-lagging-head-is-awaited-then-retryable")
	h := newProposals(t, "")
	created := h.upsert(`{"change":` + topicChange + `,"title":"Lagging"}`)
	h.push("topic", fakeSHA(3), 2)
	caught := h.upsert(`{"change":{"base":"main","head":"topic","headCommit":"` + fakeSHA(3) + `"},"title":"Lagging",` +
		`"expectedRevision":"` + created.Proposal.Revision + `"}`)
	if caught.Proposal.Change.HeadCommit != fakeSHA(3) || caught.Proposal.Revision == created.Proposal.Revision {
		t.Fatalf("the upsert answered before the head caught up: %+v", caught.Proposal)
	}
	h.push("topic", fakeSHA(4), 100)
	failure := h.fails("upsert", `{"change":{"base":"main","head":"topic","headCommit":"`+fakeSHA(4)+`"},"title":"Lagging"}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonHeadLagging || !failure.Retryable {
		t.Fatalf("a head that stays behind: %+v", failure)
	}
	failure = h.fails("find", `{"change":{"base":"main","head":"topic","headCommit":"`+fakeSHA(4)+`"}}`, collab.OutcomeUnavailable)
	if failure.Reason != reasonHeadLagging {
		t.Fatalf("find behind the branch: %+v", failure)
	}
	if puts := h.fake.count("PUT "); puts != 0 {
		t.Fatalf("a lagging head wrote %d times", puts)
	}
}

func TestALostUpsertAnswerIsReadBack(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "a-lost-upsert-answer-is-read-back")
	h := newProposals(t, "")
	h.fake.addFault("POST", `/merge_requests$`, fault{drop: true})
	created := h.upsert(`{"change":` + topicChange + `,"title":"Lost","body":"Body"}`)
	if !created.Created || h.fake.count("POST ") != 1 {
		t.Fatalf("a lost create answered %+v after %d posts", created, h.fake.count("POST "))
	}
	h.fake.addFault("PUT", `/merge_requests/1$`, fault{drop: true})
	updated := h.upsert(`{"change":` + topicChange + `,"title":"Renamed","body":"Body"}`)
	if updated.Created || updated.Proposal.Title != "Renamed" || h.fake.count("PUT ") != 1 {
		t.Fatalf("a lost update answered %+v", updated)
	}
	h.fake.addFault("PUT", `/merge_requests/1$`, fault{status: 502, body: `{"message":"bad gateway"}`, before: true})
	failure := h.fails("upsert", `{"change":`+topicChange+`,"title":"Unknown","body":"Body"}`, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" {
		t.Fatalf("an unestablished update: %+v", failure)
	}
	// GitLab refuses a second open merge request; the provider answers the
	// one that exists.
	h.fake.addFault("GET", `/merge_requests$`, fault{status: 200, body: `[]`, before: true})
	again := h.upsert(`{"change":` + topicChange + `,"title":"Different","body":"Body"}`)
	if again.Created || again.Proposal.Ref != created.Proposal.Ref || again.Proposal.Title != "Different" || h.mrCount() != 1 {
		t.Fatalf("a create GitLab refused answered %+v", again)
	}
}

func TestTheRevisionCoversWhatAProposalShows(t *testing.T) {
	spectest.Proves(t, feature, "exact-proposal-identity", "the-revision-covers-what-a-proposal-shows")
	h := newProposals(t, "")
	created := h.upsert(`{"change":` + topicChange + `,"title":"Revised"}`)
	iid := mrIID(t, created.Proposal.Ref)
	status := func() collab.Proposal {
		var result collab.ProposalStatusResult
		h.ok("status", `{"ref":`+refJSON(created.Proposal.Ref)+`}`, &result)
		return result.Proposal
	}
	before := status()
	h.mr(iid, func(mr *fakeMR) { h.fake.addNote(mr, "A note", otherID) })
	if after := status(); after.Revision != before.Revision {
		t.Fatal("a note moved the revision")
	}
	for name, change := range map[string]func(*fakeMR){
		"labels":    func(mr *fakeMR) { mr.labels = []string{"x"} },
		"assignees": func(mr *fakeMR) { mr.assignees = []int64{9} },
		"head":      func(mr *fakeMR) { mr.sha = fakeSHA(8) },
		"state":     func(mr *fakeMR) { mr.state = "closed" },
	} {
		previous := status().Revision
		h.mr(iid, change)
		if status().Revision == previous {
			t.Errorf("a change of %s kept the revision", name)
		}
	}
}

func TestDraftIsATitlePrefixNoTitleShows(t *testing.T) {
	spectest.Proves(t, feature, "draft-by-title", "draft-is-a-title-prefix-no-title-shows")
	h := newProposals(t, "")
	draft := h.upsert(`{"change":` + topicChange + `,"title":"Work","draft":true}`)
	if draft.Proposal.State != collab.ProposalStateDraft || draft.Proposal.Title != "Work" {
		t.Fatalf("a draft answered %+v", draft.Proposal)
	}
	h.mr(1, func(mr *fakeMR) {
		if mr.title != "Draft: Work" {
			t.Fatalf("GitLab stores %q", mr.title)
		}
	})
	ready := h.upsert(`{"change":` + topicChange + `,"title":"Work","draft":false}`)
	if ready.Proposal.State != collab.ProposalStateOpen || ready.Proposal.Title != "Work" {
		t.Fatalf("ready answered %+v", ready.Proposal)
	}
	// A draft another client set with GitLab's other spelling reads without it.
	h.mr(1, func(mr *fakeMR) { mr.title = "[Draft] Work" })
	var found collab.ProposalListResult
	h.ok("find", `{"change":`+topicChange+`}`, &found)
	if found.Items[0].Title != "Work" || found.Items[0].State != collab.ProposalStateDraft {
		t.Fatalf("a draft by another prefix reads %+v", found.Items[0])
	}
}

func TestAnUpsertWithoutDraftKeepsTheDraftState(t *testing.T) {
	spectest.Proves(t, feature, "draft-by-title", "an-upsert-without-draft-keeps-the-draft-state")
	h := newProposals(t, "")
	h.upsert(`{"change":` + topicChange + `,"title":"Work","draft":true}`)
	renamed := h.upsert(`{"change":` + topicChange + `,"title":"Renamed"}`)
	if renamed.Proposal.State != collab.ProposalStateDraft || renamed.Proposal.Title != "Renamed" {
		t.Fatalf("a rename answered %+v", renamed.Proposal)
	}
	h.mr(1, func(mr *fakeMR) {
		if mr.title != "Draft: Renamed" {
			t.Fatalf("GitLab stores %q", mr.title)
		}
	})
	puts := h.fake.count("PUT ")
	h.upsert(`{"change":` + topicChange + `,"title":"Renamed"}`)
	if h.fake.count("PUT ") != puts {
		t.Fatal("an unchanged upsert wrote")
	}
}

func TestATitleWithADraftPrefixIsInvalid(t *testing.T) {
	spectest.Proves(t, feature, "draft-by-title", "a-title-with-a-draft-prefix-is-invalid")
	h := newProposals(t, "")
	for _, title := range []string{"Draft: x", "draft:x", "[Draft] x", "(DRAFT) x", "Draft - x", "Draft"} {
		failure := h.fails("upsert", `{"change":`+topicChange+`,"title":"`+title+`"}`, collab.OutcomeInvalid)
		if failure.Reason != reasonTitleDraft {
			t.Fatalf("%q: %+v", title, failure)
		}
	}
	if calls := h.fake.count(""); calls != 0 {
		t.Fatalf("refused titles sent %d calls", calls)
	}
	h.upsert(`{"change":` + topicChange + `,"title":"Drafting the parser"}`)
}

func TestADescriptionOrReviewRunsNoQuickAction(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "a-description-or-review-runs-no-quick-action")
	h := newProposals(t, "")
	h.set(func(f *fakeGitLab) { f.authorMayApprove = true })
	body := "Intro\n/close\n/merge\n```\n/approve\n```"
	created := h.upsert(`{"change":` + topicChange + `,"title":"Literal","body":` + strconv.Quote(body) + `}`)
	if created.Proposal.Body != body || created.Proposal.State != collab.ProposalStateOpen {
		t.Fatalf("a literal description answered %+v", created.Proposal)
	}
	var review collab.ProposalReviewResult
	h.ok("review", `{"ref":`+refJSON(created.Proposal.Ref)+`,"verdict":"comment","body":"/approve\n/close","idempotencyKey":"literal"}`, &review)
	var status collab.ProposalStatusResult
	h.ok("status", `{"ref":`+refJSON(created.Proposal.Ref)+`}`, &status)
	if status.Proposal.State != collab.ProposalStateOpen || status.Proposal.Body != body {
		t.Fatalf("a quick action ran: %+v", status.Proposal)
	}
	for _, listed := range status.Reviews {
		if listed.Verdict == collab.ReviewVerdictApprove {
			t.Fatalf("a review body approved: %+v", status.Reviews)
		}
	}
	var comments collab.ProposalCommentListResult
	h.ok("comments", `{"ref":`+refJSON(created.Proposal.Ref)+`}`, &comments)
	if len(comments.Items) != 1 || comments.Items[0].Body != "/approve\n/close" {
		t.Fatalf("the review body reads back as %+v", comments.Items)
	}
}
