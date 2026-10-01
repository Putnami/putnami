package providertest

import (
	"slices"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// RunProposals runs the proposals v1 scenarios, each against a fresh target.
// Each scenario may open proposals on the target's two heads, so a target
// against a real backend supplies fresh heads for every scenario.
func RunProposals(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(t *testing.T, s *Session)
	}{
		{"an unused identity finds nothing", proposalsEmpty},
		{"upsert keeps one open proposal per exact identity", proposalsUpsertOnce},
		{"each exact head has its own proposal", proposalsExactIdentity},
		{"status reports reviews, and a review key replays", proposalsStatusAndReviews},
		{"a reference from another source is not found", proposalsForeignReferences},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.run(t, Open(t, newTarget(t)))
		})
	}
}

func (s *Session) change(head int) string {
	return `{"base":` + quote(s.target.Base) + `,"head":` + quote(s.target.Heads[head]) + `}`
}

func (s *Session) upsert(head int, title, extra string) collab.ProposalUpsertResult {
	s.t.Helper()
	var result collab.ProposalUpsertResult
	s.OK("proposals", "upsert", `{"change":`+s.change(head)+`,"title":`+quote(title)+`,"body":"Opened by the provider contract scenarios."`+extra+`}`, &result)
	s.sameChange(result.Proposal, head)
	if result.Created {
		s.track("proposals", result.Proposal.Ref)
	}
	return result
}

// sameChange fails unless a proposal is about the exact base and head the
// scenario named, in a repository the provider named.
func (s *Session) sameChange(proposal collab.Proposal, head int) {
	s.t.Helper()
	change := proposal.Change
	if change.Base != s.target.Base || change.Head != s.target.Heads[head] || change.Repository == "" {
		s.t.Fatalf("proposal %s/%s is about %s %s...%s, requested %s...%s", proposal.Ref.Source, proposal.Ref.ID,
			change.Repository, change.Base, change.Head, s.target.Base, s.target.Heads[head])
	}
}

// sameMembers fails unless two answers about one proposal report its labels
// and assignees alike: both absent (the provider does not report them), or
// both present with the same members.
func (s *Session) sameMembers(answered, read collab.Proposal) {
	s.t.Helper()
	for name, lists := range map[string][2][]string{"labels": {answered.Labels, read.Labels}, "assignees": {answered.Assignees, read.Assignees}} {
		if (lists[0] == nil) != (lists[1] == nil) || !slices.Equal(lists[0], lists[1]) {
			s.t.Fatalf("proposal %s/%s: %s answered as %q and read back as %q", answered.Ref.Source, answered.Ref.ID, name, lists[0], lists[1])
		}
	}
}

func proposalsEmpty(t *testing.T, s *Session) {
	var page collab.ProposalListResult
	s.OK("proposals", "find", `{"change":`+s.change(0)+`}`, &page)
	if page.Items == nil || len(page.Items) != 0 || page.Page.Next != "" {
		t.Fatalf("an identity nothing was opened for matched %+v", page)
	}
}

func proposalsUpsertOnce(t *testing.T, s *Session) {
	run := Unique(t)
	created := s.upsert(0, "Scenario proposal "+run, "")
	if !created.Created || created.Proposal.State != collab.ProposalStateOpen || created.Proposal.Title != "Scenario proposal "+run {
		t.Fatalf("created %+v", created)
	}
	updated := s.upsert(0, "Renamed proposal "+run, `,"expectedRevision":`+quote(created.Proposal.Revision))
	if updated.Created || updated.Proposal.Ref != created.Proposal.Ref || updated.Proposal.Title != "Renamed proposal "+run ||
		updated.Proposal.Revision == created.Proposal.Revision {
		t.Fatalf("a repeated upsert answered %+v after %+v", updated, created)
	}
	failure := s.Fails("proposals", "upsert", `{"change":`+s.change(0)+`,"title":"Stale","expectedRevision":`+quote(created.Proposal.Revision)+`}`,
		collab.OutcomeConflict)
	if failure.Retryable || (failure.Current != "" && failure.Current != updated.Proposal.Revision) {
		t.Fatalf("a stale upsert: %+v, current revision %s", failure, updated.Proposal.Revision)
	}
	var found collab.ProposalListResult
	s.OK("proposals", "find", `{"change":`+s.change(0)+`}`, &found)
	if len(found.Items) != 1 || found.Items[0].Ref != created.Proposal.Ref || found.Items[0].Revision != updated.Proposal.Revision {
		t.Fatalf("find matched %+v; want the one proposal at its latest revision", found.Items)
	}
	s.sameChange(found.Items[0], 0)
	s.sameMembers(updated.Proposal, found.Items[0])
	s.OK("proposals", "find", `{"change":`+s.change(0)+`,"states":["merged","closed"]}`, &found)
	if len(found.Items) != 0 {
		t.Fatalf("a state filter matched %+v", found.Items)
	}
}

func proposalsExactIdentity(t *testing.T, s *Session) {
	run := Unique(t)
	first := s.upsert(0, "First head "+run, "")
	second := s.upsert(1, "Second head "+run, "")
	if !second.Created || second.Proposal.Ref == first.Proposal.Ref {
		t.Fatalf("another head shares a proposal: %+v and %+v", first, second)
	}
	for head, want := range []collab.Ref{first.Proposal.Ref, second.Proposal.Ref} {
		var found collab.ProposalListResult
		s.OK("proposals", "find", `{"change":`+s.change(head)+`}`, &found)
		if len(found.Items) != 1 || found.Items[0].Ref != want {
			t.Fatalf("find for head %s matched %+v", s.target.Heads[head], found.Items)
		}
		s.sameChange(found.Items[0], head)
	}
}

func proposalsStatusAndReviews(t *testing.T, s *Session) {
	run := Unique(t)
	proposal := s.upsert(0, "Reviewed proposal "+run, "").Proposal
	ref := refJSON(proposal.Ref)
	key := "providertest-review-" + run
	var review, replay collab.ProposalReviewResult
	s.OK("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Scenario review.","idempotencyKey":`+quote(key)+`}`, &review)
	if !review.Created || review.Review.Verdict != collab.ReviewVerdictComment {
		t.Fatalf("review answered %+v", review)
	}
	s.OK("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Scenario review.","idempotencyKey":`+quote(key)+`}`, &replay)
	if replay.Created || replay.Review.Ref != review.Review.Ref {
		t.Fatalf("a repeated review published a second one: %+v then %+v", review, replay)
	}
	failure := s.Fails("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Other text.","idempotencyKey":`+quote(key)+`}`,
		collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch {
		t.Fatalf("a reused review key with other content: %+v", failure)
	}

	var status collab.ProposalStatusResult
	s.OK("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Proposal.Ref != proposal.Ref {
		t.Fatalf("status answered for %+v, requested %+v", status.Proposal.Ref, proposal.Ref)
	}
	s.sameChange(status.Proposal, 0)
	s.sameMembers(proposal, status.Proposal)
	published := 0
	for _, listed := range status.Reviews {
		if listed.Ref == review.Review.Ref {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("status lists the review %d times: %+v", published, status.Reviews)
	}
	if status.Checks.State == collab.ChecksStateUnsupported && status.Checks.Detail == "" {
		t.Fatal("a provider without hosted checks says why")
	}
	if !slices.Contains(collab.ValidChecksStates, status.Checks.State) {
		t.Fatalf("checks state %q", status.Checks.State)
	}
}

func proposalsForeignReferences(t *testing.T, s *Session) {
	ref := refJSON(foreignRef)
	s.Fails("proposals", "status", `{"ref":`+ref+`}`, collab.OutcomeNotFound)
	s.Fails("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"x","idempotencyKey":"providertest-foreign"}`, collab.OutcomeNotFound)
}
