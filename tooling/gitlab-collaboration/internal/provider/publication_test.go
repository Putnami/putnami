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
	h.seed("Feature", "", "kind/feature", "area::core")
	h.seed("Bug", "", "kind/bug")
	h.set(func(f *fakeGitLab) {
		f.foreign["acme/platform/lib"] = map[int][]string{3: {"kind/lib"}}
		f.foreign["acme/other"] = map[int][]string{4: {"kind/other", "unrelated"}}
	})
}

func TestLabelsAreInheritedFromGitLabReferences(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "labels-are-inherited-from-gitlab-references")
	h := newProposals(t, `"inheritLabelPrefixes":["kind/"]`)
	related(h)
	body := "Closes #1\nRefs: https://" + fakeHost + "/acme/platform/app/-/issues/2.\nRelated to lib#3, fixes acme/other#4 and closes #1 again"
	created := h.upsert(`{"change":` + topicChange + `,"title":"Inherits","body":` + strconv.Quote(body) + `}`)
	want := []string{"kind/bug", "kind/feature", "kind/lib", "kind/other"}
	if !slices.Equal(created.Proposal.Labels, want) || created.Proposal.Body != body {
		t.Fatalf("labels %v, want %v", created.Proposal.Labels, want)
	}
	if reads := h.fake.count("GET /api/v4/projects/acme/platform/app/issues/1"); reads != 1 {
		t.Fatalf("#1 was read %d times", reads)
	}
}

func TestAMergeRequestReferenceOrForeignURLAddsNothing(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-merge-request-reference-or-foreign-url-adds-nothing")
	h := newProposals(t, `"inheritLabelPrefixes":["kind/"]`)
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
	mustContain(t, failure.Reconcile, "correct or remove the reference")
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
	if h.fake.count("PUT /api/v4/projects/acme/platform/app/merge_requests/1/merge") != 1 {
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
	h.set(func(f *fakeGitLab) { f.unmergeable, f.mergeError = false, "Merge request is not mergeable" })
	failure = h.fails("merge", `{"ref":`+refJSON(created.Proposal.Ref)+`,"expectedHeadCommit":"`+fakeSHA(3)+`"}`, collab.OutcomeConflict)
	if failure.Reason != reasonNotMergeable {
		t.Fatalf("a merge GitLab answered with merge_error: %+v", failure)
	}
	// The same error the merge request already carried is no answer to this
	// attempt: the merge request is read back.
	failure = h.fails("merge", `{"ref":`+refJSON(created.Proposal.Ref)+`,"expectedHeadCommit":"`+fakeSHA(3)+`"}`, collab.OutcomeUnresolved)
	if failure.Retryable || failure.Reconcile == "" {
		t.Fatalf("a stale merge_error: %+v", failure)
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

func TestASingleAssigneeTierKeepsItsAssignee(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-single-assignee-tier-keeps-its-assignee")
	h := newProposals(t, `"assignAuthor":true`)
	h.set(func(f *fakeGitLab) { f.singleAssignee = true })
	created := h.upsert(`{"change":` + topicChange + `,"title":"Single"}`)
	if !slices.Equal(created.Proposal.Assignees, []string{"bot"}) {
		t.Fatalf("an unassigned merge request: %v", created.Proposal.Assignees)
	}
	h.mr(1, func(mr *fakeMR) { mr.assignees = []int64{9} })
	kept := h.upsert(`{"change":` + topicChange + `,"title":"Single"}`)
	if !slices.Equal(kept.Proposal.Assignees, []string{"alice"}) {
		t.Fatalf("the present assignee was not kept: %v", kept.Proposal.Assignees)
	}
	// Keeping one of two present assignees would drop the other.
	h.mr(1, func(mr *fakeMR) { mr.assignees = []int64{9, 10} })
	failure := h.fails("upsert", `{"change":`+topicChange+`,"title":"Single"}`, collab.OutcomeUnresolved)
	if failure.Reason != reasonIncomplete {
		t.Fatalf("a dropped assignee: %+v", failure)
	}
	mustContain(t, failure.Message, "kept alice and removed bob")
	if again := h.upsert(`{"change":` + topicChange + `,"title":"Single"}`); !slices.Equal(again.Proposal.Assignees, []string{"alice"}) {
		t.Fatalf("a repeat answered %v", again.Proposal.Assignees)
	}
}

func TestAScopedLabelKeyTheMergeRequestHasIsKept(t *testing.T) {
	spectest.Proves(t, feature, "reported-publication", "a-scoped-label-key-the-merge-request-has-is-kept")
	h := newProposals(t, `"inheritLabelPrefixes":["type::","area::"]`)
	h.set(func(f *fakeGitLab) { f.scopedLabels = true })
	h.seed("Feature", "", "type::feature", "area::core")
	h.seed("Bug", "", "type::bug", "area::cli")
	created := h.upsert(`{"change":` + topicChange + `,"title":"Scoped","body":"Closes #1 and closes #2"}`)
	if !slices.Equal(created.Proposal.Labels, []string{"area::core", "type::feature"}) {
		t.Fatalf("labels %v", created.Proposal.Labels)
	}
	h.branch("other", fakeSHA(5))
	h.seedMR("other", "main", "Labeled", botID, h.fake.projectID).labels = []string{"type::chore"}
	labeled := h.upsert(`{"change":{"base":"main","head":"other"},"title":"Labeled","body":"Closes #2"}`)
	if !slices.Equal(labeled.Proposal.Labels, []string{"area::cli", "type::chore"}) {
		t.Fatalf("an addition replaced a scoped label: %v", labeled.Proposal.Labels)
	}
}

func TestASquashOptionTheProjectForbidsIsUnsupported(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "a-squash-option-the-project-forbids-is-unsupported")
	h := newProposals(t, `"allowMerge":true`)
	created := h.upsert(`{"change":` + topicChange + `,"title":"Squash"}`)
	h.set(func(f *fakeGitLab) { f.squashOption = "never" })
	failure := h.fails("merge", h.merge(created.Proposal.Ref, `,"method":"squash"`), collab.OutcomeUnsupported)
	if failure.Reason != reasonMergeMethod {
		t.Fatalf("a squash the project never does: %+v", failure)
	}
	h.set(func(f *fakeGitLab) { f.squashOption = "always" })
	h.fails("merge", h.merge(created.Proposal.Ref, `,"method":"merge"`), collab.OutcomeUnsupported)
	if puts := h.fake.count("PUT "); puts != 0 {
		t.Fatalf("an unsupported method sent %d merges", puts)
	}
	var merged collab.ProposalResult
	h.ok("merge", h.merge(created.Proposal.Ref, `,"method":"squash"`), &merged)
}

func TestTheMergeRequestSquashFlagFollowsTheMethod(t *testing.T) {
	spectest.Proves(t, feature, "guarded-merge", "the-merge-request-squash-flag-follows-the-method")
	h := newProposals(t, `"allowMerge":true`)
	// A project that encourages squashing sets the flag on every create, and
	// a merge call never clears it.
	h.set(func(f *fakeGitLab) { f.squashOption = "default_on" })
	created := h.upsert(`{"change":` + topicChange + `,"title":"Kept commits"}`)
	var merged collab.ProposalResult
	h.ok("merge", h.merge(created.Proposal.Ref, `,"method":"merge"`), &merged)
	h.mr(1, func(mr *fakeMR) {
		if mr.squashed || mr.squash {
			t.Fatalf("a merge commit was squashed: flag %t", mr.squash)
		}
	})
	// A protected branch's rule overrides the merge request's flag: nothing
	// is merged.
	h.branch("other", fakeSHA(2))
	other := h.upsert(`{"change":{"base":"main","head":"other"},"title":"Squashed"}`)
	h.set(func(f *fakeGitLab) { f.squashOption, f.branchSquash = "default_off", "never" })
	failure := h.fails("merge", h.merge(other.Proposal.Ref, `,"method":"squash"`), collab.OutcomeUnsupported)
	if failure.Reason != reasonMergeMethod {
		t.Fatalf("a squash a branch rule forbids: %+v", failure)
	}
	if merges := h.fake.count("PUT /api/v4/projects/acme/platform/app/merge_requests/2/merge"); merges != 0 {
		t.Fatalf("an overridden method merged %d times", merges)
	}
}
