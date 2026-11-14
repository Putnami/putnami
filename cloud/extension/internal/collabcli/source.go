package collabcli

import (
	"context"
	"errors"

	collab "go.putnami.dev/protocol/collaboration"
)

// Source is the collaboration surface of Putnami Source the provider calls:
// one method per served operation, each one request to a route under
// /v1/workspaces/{workspace}/source/ of the workspace the session resolved.
// Source speaks the collaboration documents itself, so a method takes the
// operation's input and answers its result.
//
// A method that fails returns a *Refusal when Source answered, an error
// wrapped by NotSent when the request never left, and any other error when
// Source's answer is unknown: a broken connection, a passed deadline, or an
// answer outside the contract.
type Source interface {
	FindProposals(ctx context.Context, input *collab.ProposalFindInput) (*collab.ProposalListResult, error)
	UpsertProposal(ctx context.Context, input *collab.ProposalUpsertInput) (*collab.ProposalUpsertResult, error)
	ProposalStatus(ctx context.Context, input *collab.ProposalRefInput) (*collab.ProposalStatusResult, error)
	ProposalComments(ctx context.Context, input *collab.ProposalCommentsInput) (*collab.ProposalCommentListResult, error)
	ReviewProposal(ctx context.Context, input *collab.ProposalReviewInput) (*collab.ProposalReviewResult, error)
	AutoMergeProposal(ctx context.Context, input *collab.ProposalAutoMergeInput) (*collab.ProposalResult, error)
	GetTask(ctx context.Context, input *collab.TaskRefInput) (*collab.TaskResult, error)
	TransitionTask(ctx context.Context, input *collab.TaskTransitionInput) (*collab.TaskResult, error)
	AssignTask(ctx context.Context, input *collab.TaskAssignInput) (*collab.TaskResult, error)
}

// Refusal is an answer of Source that refused the request: nothing was
// written. Status is the HTTP status; Reason, Message and Current are what
// Source said, when it said it.
type Refusal struct {
	Status  int
	Reason  string
	Message string
	Current string
}

func (r *Refusal) Error() string {
	if r.Message != "" {
		return r.Message
	}
	return "Putnami Source refused the request"
}

// notSent marks a failure that happened before the request left.
type notSent struct{ err error }

func (n notSent) Error() string { return n.err.Error() }
func (n notSent) Unwrap() error { return n.err }

// NotSent marks err as a failure before the request left: a write that fails
// this way is known not to have happened.
func NotSent(err error) error {
	if err == nil {
		return nil
	}
	return notSent{err: err}
}

func wasNotSent(err error) bool {
	var marked notSent
	return errors.As(err, &marked)
}
