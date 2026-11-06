package provider

import (
	"slices"
	"strconv"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestAssignAuthorAddsTheAccount(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "assign-author-adds-the-account")
	h := newProposals(t, `"assignAuthor":true`)
	plain := newProposals(t, "")
	if created := plain.upsert(`{"change":` + topicChange + `,"title":"Plain"}`); created.Proposal.Assignees == nil || len(created.Proposal.Assignees) != 0 ||
		created.Proposal.Labels == nil {
		t.Fatalf("a proposal without additions reports %+v", created.Proposal)
	}
	created := h.upsert(`{"change":` + topicChange + `,"title":"Assigned"}`)
	if !slices.Equal(created.Proposal.Assignees, []string{"bot"}) {
		t.Fatalf("assignees %v", created.Proposal.Assignees)
	}
	h.mr(1, func(mr *fakeMR) { mr.assignees = []int64{9} })
	again := h.upsert(`{"change":` + topicChange + `,"title":"Assigned"}`)
	if !slices.Equal(again.Proposal.Assignees, []string{"alice", "bot"}) {
		t.Fatalf("an addition replaced the assignees: %v", again.Proposal.Assignees)
	}
	puts := h.fake.count("PUT ")
	h.upsert(`{"change":` + topicChange + `,"title":"Assigned"}`)
	if h.fake.count("PUT ") != puts {
		t.Fatal("an assigned account was assigned again")
	}
}

// related seeds issues the description of a proposal can name: #1 and #2 in
// the bound project, #3 of acme/platform/lib, #4 of acme/other.
func related(h *harness) {
	h.seed("Feature", "", "type::feature", "area::core")
	h.seed("Bug", "", "type::bug")
	h.set(func(f *fakeGitLab) {
		f.foreign["acme/platform/lib"] = map[int][]string{3: {"type::lib"}}
		f.foreign["acme/other"] = map[int][]string{4: {"type::other", "unrelated"}}
	})
}

func TestLabelsAreInheritedFromGitLabReferences(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "labels-are-inherited-from-gitlab-references")
	h := newProposals(t, `"inheritLabelPrefixes":["type::"]`)
	related(h)
	body := "Closes #1\nRefs: https://" + fakeHost + "/acme/platform/app/-/issues/2.\nRelated to lib#3, fixes acme/other#4 and closes #1 again"
	created := h.upsert(`{"change":` + topicChange + `,"title":"Inherits","body":` + strconv.Quote(body) + `}`)
	want := []string{"type::bug", "type::feature", "type::lib", "type::other"}
	if !slices.Equal(created.Proposal.Labels, want) || created.Proposal.Body != body {
		t.Fatalf("labels %v, want %v", created.Proposal.Labels, want)
	}
	if reads := h.fake.count("GET /api/v4/projects/acme/platform/app/issues/1"); reads != 1 {
		t.Fatalf("#1 was read %d times", reads)
	}
}

func TestAMergeRequestReferenceOrForeignURLAddsNothing(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-merge-request-reference-or-foreign-url-adds-nothing")
	h := newProposals(t, `"inheritLabelPrefixes":["type::"]`)
	related(h)
	body := "Closes !1\nCloses acme/platform/app!2\nCloses https://gitlab.com/acme/platform/app/-/issues/1\n" +
		"Closes http://" + fakeHost + "/acme/platform/app/-/issues/1\nCloses #1x and refs #2-old"
	created := h.upsert(`{"change":` + topicChange + `,"title":"Nothing","body":` + strconv.Quote(body) + `}`)
	if len(created.Proposal.Labels) != 0 || h.fake.count("GET /api/v4/projects/acme/platform/app/issues/") != 0 {
		t.Fatalf("labels %v", created.Proposal.Labels)
	}
}

func TestAFailedAdditionIsUnresolvedAndNamesTheMergeRequest(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-failed-addition-is-unresolved-and-names-the-merge-request")
	h := newProposals(t, `"inheritLabelPrefixes":["type::"]`)
	failure := h.fails("upsert", `{"change":`+topicChange+`,"title":"Missing","body":"Closes #99"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete || failure.Retryable {
		t.Fatalf("a missing related issue: %+v", failure)
	}
	mustContain(t, failure.Message, "merge request !1 of "+fakeProject+" was written")
	refused := newProposals(t, `"assignAuthor":true`)
	refused.fake.addFault("PUT", `/merge_requests/1$`, statusFault(403, "403 Forbidden", true))
	failure = refused.fails("upsert", `{"change":`+topicChange+`,"title":"Refused"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete {
		t.Fatalf("a refused assignment: %+v", failure)
	}
	mustContain(t, failure.Reconcile, "settings.assignAuthor")
}

func (h *harness) merge(ref collab.Ref, extra string) string {
	return `{"ref":` + refJSON(ref) + `,"expectedHeadCommit":"` + fakeSHA(2) + `"` + extra + `}`
}

func TestMergeIsDeniedUnlessAllowed(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "merge-is-denied-unless-allowed")
	h := newProposals(t, "")
	failure := h.fails("merge", h.merge(mrRef(1), ""), collab.OutcomeDenied)
	if failure.Reason != reasonMergeDisabled || h.fake.count("") != 0 {
		t.Fatalf("an unallowed merge: %+v after %d calls", failure, h.fake.count(""))
	}
	h.settings = proposalSettings(`"allowMerge":false`)
	h.fails("merge", h.merge(mrRef(1), ""), collab.OutcomeDenied)
}

func TestMergeSendsTheExpectedHead(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "merge-sends-the-expected-head")
	h := newProposals(t, `"allowMerge":true`)
	created := h.upsert(`{"change":` + topicChange + `,"title":"Merged"}`)
	var merged collab.ProposalResult
	h.ok("merge", h.merge(created.Proposal.Ref, `,"method":"squash"`), &merged)
	if merged.Proposal.State != collab.ProposalStateMerged {
		t.Fatalf("merged %+v", merged.Proposal)
	}
	h.set(func(f *fakeGitLab) {
		sent := f.mergeRequests[0]
		if sent.SHA == nil || *sent.SHA != fakeSHA(2) || sent.Squash == nil || !*sent.Squash || sent.RemoveSourceBranch != nil {
			t.Fatalf("the merge call sent %+v", sent)
		}
	})
	// A merge already done answers the merged proposal without a call.
	h.ok("merge", h.merge(created.Proposal.Ref, ""), &merged)
	if h.fake.count("PUT ") != 1 {
		t.Fatal("a merged merge request was merged again")
	}
	// A merge whose answer is lost is read back.
	h.branch("lost", fakeSHA(2))
	lost := h.upsert(`{"change":{"base":"main","head":"lost"},"title":"Lost"}`)
	h.fake.addFault("PUT", `/merge$`, fault{drop: true})
	h.ok("merge", h.merge(lost.Proposal.Ref, ""), &merged)
	if merged.Proposal.State != collab.ProposalStateMerged {
		t.Fatalf("a lost merge answered %+v", merged.Proposal)
	}
}

func TestAMovedHeadOrUnmergeableRequestIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "a-moved-head-or-unmergeable-request-is-a-conflict")
	h := newProposals(t, `"allowMerge":true`)
	created := h.upsert(`{"change":` + topicChange + `,"title":"Conflicted"}`)
	failure := h.fails("merge", `{"ref":`+refJSON(created.Proposal.Ref)+`,"expectedHeadCommit":"`+fakeSHA(9)+`"}`, collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved || h.fake.count("PUT ") != 0 {
		t.Fatalf("a moved head: %+v", failure)
	}
	// The head moves between the read and the merge: GitLab refuses the sha.
	h.push("topic", fakeSHA(3), 1)
	failure = h.fails("merge", h.merge(created.Proposal.Ref, ""), collab.OutcomeConflict)
	if failure.Reason != reasonHeadMoved {
		t.Fatalf("a head that moved meanwhile: %+v", failure)
	}
	h.set(func(f *fakeGitLab) { f.unmergeable = true })
	failure = h.fails("merge", `{"ref":`+refJSON(created.Proposal.Ref)+`,"expectedHeadCommit":"`+fakeSHA(3)+`"}`, collab.OutcomeConflict)
	if failure.Reason != reasonNotMergeable {
		t.Fatalf("an unmergeable merge request: %+v", failure)
	}
	h.mr(1, func(mr *fakeMR) { mr.state = "closed" })
	failure = h.fails("merge", `{"ref":`+refJSON(created.Proposal.Ref)+`,"expectedHeadCommit":"`+fakeSHA(3)+`"}`, collab.OutcomeConflict)
	if failure.Reason != reasonNotMergeable {
		t.Fatalf("a closed merge request: %+v", failure)
	}
}

func TestAMergeMethodTheProjectCannotHonorIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "a-merge-method-the-project-cannot-honor-is-unsupported")
	h := newProposals(t, `"allowMerge":true`)
	created := h.upsert(`{"change":` + topicChange + `,"title":"Method"}`)
	h.set(func(f *fakeGitLab) { f.mergeMethod = "ff" })
	failure := h.fails("merge", h.merge(created.Proposal.Ref, `,"method":"merge"`), collab.OutcomeUnsupported)
	if failure.Reason != reasonMergeMethod || h.fake.count("PUT ") != 0 {
		t.Fatalf("a merge commit on a fast-forward project: %+v", failure)
	}
	var merged collab.ProposalResult
	h.ok("merge", h.merge(created.Proposal.Ref, `,"method":"rebase"`), &merged)
	h.set(func(f *fakeGitLab) {
		if sent := f.mergeRequests[0]; sent.Squash == nil || *sent.Squash {
			t.Fatalf("a rebase merge sent %+v", sent)
		}
	})
	h.branch("other", fakeSHA(2))
	other := h.upsert(`{"change":{"base":"main","head":"other"},"title":"Other"}`)
	h.set(func(f *fakeGitLab) { f.mergeMethod = "rebase_merge" })
	h.fails("merge", h.merge(other.Proposal.Ref, `,"method":"rebase"`), collab.OutcomeUnsupported)
	h.ok("merge", h.merge(other.Proposal.Ref, `,"method":"merge"`), &merged)
}

func TestRemoveSourceBranchRidesOnTheMerge(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "remove-source-branch-rides-on-the-merge")
	h := newProposals(t, `"allowMerge":true,"removeSourceBranch":true`)
	created := h.upsert(`{"change":` + topicChange + `,"title":"Removed"}`)
	var merged collab.ProposalResult
	h.ok("merge", h.merge(created.Proposal.Ref, ""), &merged)
	h.set(func(f *fakeGitLab) {
		if sent := f.mergeRequests[0]; sent.RemoveSourceBranch == nil || !*sent.RemoveSourceBranch || sent.Squash != nil {
			t.Fatalf("the merge call sent %+v", sent)
		}
		if _, kept := f.branches["topic"]; kept {
			t.Fatal("the source branch is still there")
		}
	})
	if writes := h.fake.writes(); writes != 2 {
		t.Fatalf("the merge took %d writes with the create; deleting the branch needs none of its own", writes)
	}
}
