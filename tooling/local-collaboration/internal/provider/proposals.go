package provider

import (
	"context"
	"slices"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// noHostedChecks is the disclosure every proposals.status answer carries.
const noHostedChecks = "the local collaboration provider runs no hosted checks; run the workspace gate and reference its session instead"

// repositoryOf is the repository a request names, or the configured default.
func repositoryOf(change collab.Change, settings Settings) string {
	switch {
	case change.Repository != "":
		return change.Repository
	case settings.Repository != "":
		return settings.Repository
	}
	return DefaultRepository
}

// reported is a proposal as every answer carries it. The store keeps no
// labels or assignees for a proposal, so each answer reports none: [] rather
// than absent, which would mean this provider does not report them.
func reported(proposal collab.Proposal) collab.Proposal {
	proposal.Labels, proposal.Assignees = []string{}, []string{}
	return proposal
}

func sameIdentity(proposal collab.Proposal, repository, base, head string) bool {
	return proposal.Change.Repository == repository && proposal.Change.Base == base && proposal.Change.Head == head
}

func (p *Provider) findProposals(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.ProposalFindInput](call)
	if failure != nil {
		return nil, failure
	}
	s, settings, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	state, failure := readState(s)
	if failure != nil {
		return nil, failure
	}
	repository := repositoryOf(input.Change, settings)
	var matches []collab.Proposal
	for _, proposal := range state.Proposals {
		if !sameIdentity(proposal, repository, input.Change.Base, input.Change.Head) {
			continue
		}
		if len(input.States) > 0 && !slices.Contains(input.States, proposal.State) {
			continue
		}
		matches = append(matches, proposal)
	}
	items, next, failure := page(matches, func(proposal collab.Proposal) string { return proposal.Ref.ID }, input.Page)
	if failure != nil {
		return nil, failure
	}
	for i := range items {
		items[i] = reported(items[i])
	}
	return &collab.ProposalListResult{Items: items, Page: next}, nil
}

// upsertProposal keeps at most one open or draft proposal per exact
// repository/base/head: it updates that one or creates it, under the store
// lock, so a repeated or concurrent upsert never creates a second.
func (p *Provider) upsertProposal(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.ProposalUpsertInput](call)
	if failure != nil {
		return nil, failure
	}
	s, settings, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	repository := repositoryOf(input.Change, settings)
	var result collab.ProposalUpsertResult
	failure = update(s, func(state *store.State) error {
		index := slices.IndexFunc(state.Proposals, func(proposal collab.Proposal) bool {
			return sameIdentity(proposal, repository, input.Change.Base, input.Change.Head) &&
				(proposal.State == collab.ProposalStateOpen || proposal.State == collab.ProposalStateDraft)
		})
		if index < 0 {
			if input.ExpectedRevision != "" {
				return refuse(conflictOn("", "no open proposal exists for %s %s...%s, so revision %s cannot match",
					repository, input.Change.Base, input.Change.Head, input.ExpectedRevision))
			}
			state.Next.Proposal++
			proposal := collab.Proposal{
				Ref:      collab.Ref{Source: state.Source(), ID: "P-" + strconv.Itoa(state.Next.Proposal)},
				Revision: "r1",
				Change: collab.Change{Repository: repository, Base: input.Change.Base, Head: input.Change.Head,
					HeadCommit: input.Change.HeadCommit},
				Title:     input.Title,
				Body:      input.Body,
				State:     proposalState(input.Draft, collab.ProposalStateOpen),
				UpdatedAt: p.timestamp(),
			}
			state.Proposals = append(state.Proposals, proposal)
			result = collab.ProposalUpsertResult{Proposal: reported(proposal), Created: true}
			return nil
		}
		proposal := &state.Proposals[index]
		if input.ExpectedRevision != "" && proposal.Revision != input.ExpectedRevision {
			return refuse(conflictOn(proposal.Revision, "proposal %s is at revision %s, not %s",
				proposal.Ref.ID, proposal.Revision, input.ExpectedRevision))
		}
		before := requestDigest(proposal)
		proposal.Title = input.Title
		proposal.Body = input.Body
		proposal.State = proposalState(input.Draft, proposal.State)
		if input.Change.HeadCommit != "" {
			proposal.Change.HeadCommit = input.Change.HeadCommit
		}
		if requestDigest(proposal) != before {
			proposal.Revision = nextRevision(proposal.Revision)
			proposal.UpdatedAt = p.timestamp()
		}
		result = collab.ProposalUpsertResult{Proposal: reported(*proposal), Created: false}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return &result, nil
}

// proposalState applies an optional draft flag to a state.
func proposalState(draft *bool, current collab.ProposalState) collab.ProposalState {
	switch {
	case draft == nil:
		return current
	case *draft:
		return collab.ProposalStateDraft
	}
	return collab.ProposalStateOpen
}

func proposalIndex(state *store.State, ref collab.Ref) int {
	if foreign(state, ref) {
		return -1
	}
	return slices.IndexFunc(state.Proposals, func(proposal collab.Proposal) bool { return proposal.Ref.ID == ref.ID })
}

func (p *Provider) proposalStatus(_ context.Context, call collab.Call) (any, *collab.Failure) {
	input, failure := inputOf[*collab.ProposalRefInput](call)
	if failure != nil {
		return nil, failure
	}
	ref := input.Ref
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	state, failure := readState(s)
	if failure != nil {
		return nil, failure
	}
	index := proposalIndex(state, ref)
	if index < 0 {
		return nil, notFound("proposal", ref)
	}
	var reviews []collab.Review
	for _, stored := range state.Reviews {
		if stored.Proposal == ref.ID {
			reviews = append(reviews, stored.Review)
		}
	}
	if len(reviews) > collab.MaxListMembers {
		reviews = reviews[len(reviews)-collab.MaxListMembers:]
	}
	return &collab.ProposalStatusResult{
		Proposal: reported(state.Proposals[index]),
		Checks:   collab.Checks{State: collab.ChecksStateUnsupported, Detail: noHostedChecks},
		Reviews:  reviews,
	}, nil
}

func (p *Provider) reviewProposal(_ context.Context, call collab.Call) (any, *collab.Failure) {
	request, failure := inputOf[*collab.ProposalReviewInput](call)
	if failure != nil {
		return nil, failure
	}
	input := *request
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	key := keyDigest("proposals.review", input.IdempotencyKey)
	digest := requestDigest(struct {
		Ref     collab.Ref           `json:"ref"`
		Verdict collab.ReviewVerdict `json:"verdict"`
		Body    string               `json:"body"`
		Commit  string               `json:"commit"`
	}{input.Ref, input.Verdict, input.Body, input.Commit})
	var review collab.Review
	replayed := false
	failure = update(s, func(state *store.State) error {
		if proposalIndex(state, input.Ref) < 0 {
			return refuse(notFound("proposal", input.Ref))
		}
		if prior, used := state.Keys[key]; used {
			if prior.Kind != "review" || prior.Digest != digest {
				return refuse(collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
					"idempotency key %q was already used for a different request", input.IdempotencyKey))
			}
			index := slices.IndexFunc(state.Reviews, func(stored store.Review) bool { return stored.Review.Ref.ID == prior.ID })
			if index < 0 {
				return refuse(collab.Fail(collab.OutcomeUnavailable, "store.inconsistent",
					"idempotency key %q names review %s, which the store no longer holds", input.IdempotencyKey, prior.ID))
			}
			review, replayed = state.Reviews[index].Review, true
			return errNothingToWrite
		}
		state.Next.Review++
		review = collab.Review{
			Ref:         collab.Ref{Source: state.Source(), ID: "R-" + strconv.Itoa(state.Next.Review)},
			Verdict:     input.Verdict,
			Commit:      input.Commit,
			SubmittedAt: p.timestamp(),
		}
		state.Reviews = append(state.Reviews, store.Review{Proposal: input.Ref.ID, Body: input.Body, Review: review})
		state.Keys[key] = store.Key{Kind: "review", ID: review.Ref.ID, Digest: digest}
		return nil
	})
	if replayed {
		return &collab.ProposalReviewResult{Review: review, Created: false}, nil
	}
	if failure != nil {
		return nil, failure
	}
	return &collab.ProposalReviewResult{Review: review, Created: true}, nil
}
