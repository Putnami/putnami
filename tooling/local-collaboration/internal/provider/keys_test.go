package provider

import (
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// An idempotency key names one write of one operation, as it does on the
// GitHub provider: the same key given to tasks.create and proposals.review
// names two writes.
func TestAKeyNamesOneWriteOfOneOperation(t *testing.T) {
	h := newHarness(t, "")
	var task collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"t","idempotencyKey":"mission-7"}`, &task)
	var proposal collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"p"}`, &proposal)
	var review, replay collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+refJSON(proposal.Proposal.Ref)+`,"verdict":"comment","body":"b","idempotencyKey":"mission-7"}`, &review)
	if !review.Created {
		t.Fatalf("a review with a key a create used: %+v", review)
	}
	h.ok("proposals", "review", `{"ref":`+refJSON(proposal.Proposal.Ref)+`,"verdict":"comment","body":"b","idempotencyKey":"mission-7"}`, &replay)
	var again collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"t","idempotencyKey":"mission-7"}`, &again)
	if replay.Created || replay.Review.Ref != review.Review.Ref || again.Created || again.Task.Ref != task.Task.Ref {
		t.Fatalf("the repeats of each write: review %+v, task %+v", replay, again)
	}
}
