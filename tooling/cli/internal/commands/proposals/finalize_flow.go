package proposals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// publicationCandidate fixes the local bytes and provider binding used by all
// later writes. recheck repeats the read-only binding check at each boundary.
type publicationCandidate struct {
	opts                    Options
	policy                  Policy
	base, baseTip, baseSHA  string
	originalBody            []byte
	body, head, fingerprint string
	proposalCaps, taskCaps  collab.Capabilities
	task                    collab.Task
	change                  collab.Change
	expectedRevision        string
}

func (c *publicationCandidate) recheck(ctx context.Context, f Finalizer) (Verified, error) {
	return f.frozen(ctx, c.opts, c.originalBody, c.head, c.fingerprint, c.base, c.baseTip, c.baseSHA)
}

func (f Finalizer) prepare(ctx context.Context, opts Options) (publicationCandidate, error) {
	policy, err := f.optionsPolicy(opts)
	if err != nil {
		return publicationCandidate{}, err
	}
	candidate := publicationCandidate{opts: opts, policy: policy}
	if err := f.prepareLocal(&candidate); err != nil {
		return candidate, err
	}
	if err := f.prepareProviders(ctx, &candidate); err != nil {
		return candidate, err
	}
	if err := ValidatePublicationText(opts.Title, candidate.body, opts.Proof, policy); err != nil {
		return candidate, err
	}
	if err := f.prepareBinding(ctx, &candidate); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func (f Finalizer) optionsPolicy(opts Options) (Policy, error) {
	if opts.Base == "" || opts.Branch == "" || opts.Title == "" || opts.BodyFile == "" {
		return Policy{}, errors.New("--base, --branch, --title and --body-file are required")
	}
	if (opts.TaskSource == "") != (opts.TaskID == "") {
		return Policy{}, errors.New("--task-source and --task-id must be given together")
	}
	if opts.PartialDelivery && opts.TaskID == "" {
		return Policy{}, errors.New("--partial-delivery needs a task reference")
	}
	if opts.Gate != "" && opts.Gate != "ci" {
		return Policy{}, errors.New("only explicit --gate ci is supported; local proof uses --verification-file")
	}
	if opts.Draft && (opts.VerificationFile != "" || opts.Gate != "" || opts.Proof != "") {
		return Policy{}, errors.New("draft accepts no verification, gate or proof")
	}
	if !opts.Draft && (opts.VerificationFile == "") == (opts.Gate != "ci") {
		return Policy{}, errors.New("ready publication needs exactly one of --verification-file or --gate ci")
	}
	policy, err := ReadPolicy(f.Root)
	if err != nil {
		return policy, err
	}
	if opts.Gate == "ci" && (len(policy.Verification.CIGate.Checks) == 0 || policy.Publication.Draft) {
		return policy, errors.New("--gate ci requires named hosted checks and a ready publication policy")
	}
	return policy, nil
}

func (f Finalizer) prepareLocal(c *publicationCandidate) error {
	var err error
	c.base, err = f.Git.BaseRef(c.opts.Base)
	if err != nil {
		return err
	}
	c.baseTip, err = f.Git.BaseTip(c.base)
	if err != nil {
		return err
	}
	c.baseSHA, err = f.Git.BaseSHA(c.base)
	if err != nil {
		return err
	}
	branch, err := f.Git.Branch()
	if err != nil {
		return err
	}
	if branch != c.opts.Branch {
		return fmt.Errorf("current branch %q differs from %q", branch, c.opts.Branch)
	}
	paths, err := f.Git.ChangedPaths(c.base)
	if err != nil {
		return err
	}
	if err := ValidateTitle(c.opts.Title, paths); err != nil {
		return err
	}
	c.originalBody, err = os.ReadFile(c.opts.BodyFile)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(c.originalBody))) == 0 {
		return errors.New("body file is empty")
	}
	c.body = NormalizeBody(string(c.originalBody))
	if c.opts.PartialDelivery && (PartialClosing(c.opts.Title) || PartialClosing(c.body)) {
		return errors.New("partial delivery refuses a closing directive in the title or body")
	}
	return f.checkBranchMessages(c)
}

func (f Finalizer) checkBranchMessages(c *publicationCandidate) error {
	commits, err := f.Git.CommitMessages(c.base)
	if err != nil {
		return err
	}
	for _, commit := range commits {
		if c.opts.PartialDelivery && !commit.Empty && PartialClosing(commit.Text) {
			return errors.New("partial delivery refuses a closing directive in a nonempty branch commit")
		}
	}
	for _, commit := range commits {
		if agentMarker.MatchString(commit.Text) {
			return errors.New("branch commit names an agent")
		}
	}
	return nil
}

func (f Finalizer) prepareProviders(ctx context.Context, c *publicationCandidate) error {
	var err error
	c.proposalCaps, err = f.capabilities(ctx, collab.ContractProposals, collab.OperationFind, collab.OperationUpsert, collab.OperationStatus)
	if err != nil {
		return err
	}
	if c.opts.TaskID == "" {
		return nil
	}
	needed := []string{collab.OperationGet}
	if !c.opts.Draft && !c.opts.PartialDelivery {
		needed = append(needed, collab.OperationTransition)
	}
	c.taskCaps, err = f.capabilities(ctx, collab.ContractTasks, needed...)
	if err != nil {
		return err
	}
	var found collab.TaskResult
	ref := collab.Ref{Source: c.opts.TaskSource, ID: c.opts.TaskID}
	if err := f.call(ctx, collab.ContractTasks, collab.OperationGet, collab.TaskRefInput{Ref: ref}, &found); err != nil {
		return err
	}
	if found.Task.Ref != ref {
		return errors.New("tasks.get returned a different task reference")
	}
	c.task = found.Task
	line, err := BoundTaskReference(c.policy, c.task, c.opts.PartialDelivery, c.proposalCaps)
	if err != nil {
		return err
	}
	if line != "" && !strings.Contains("\n"+c.body+"\n", "\n"+line+"\n") {
		c.body += "\n\n" + line
	}
	return nil
}

func (f Finalizer) prepareBinding(ctx context.Context, c *publicationCandidate) error {
	if c.opts.Draft {
		staged, err := f.Git.Staged()
		if err != nil {
			return err
		}
		if staged {
			return errors.New("draft seed refuses staged changes")
		}
	} else {
		clean, err := f.Git.Clean()
		if err != nil {
			return err
		}
		if !clean {
			return errors.New("ready finalization requires a clean committed tree")
		}
	}
	var err error
	c.head, err = f.Git.Head()
	if err != nil {
		return err
	}
	if !c.opts.Draft {
		c.fingerprint, err = f.Git.Fingerprint()
		if err != nil {
			return err
		}
	}
	_, err = c.recheck(ctx, f)
	return err
}

func (f Finalizer) seedAndFind(ctx context.Context, c *publicationCandidate) error {
	ahead, err := f.Git.Ahead(c.base)
	if err != nil {
		return err
	}
	if c.opts.Draft && !ahead {
		if err := f.Git.Seed(c.opts.Title); err != nil {
			return err
		}
		c.head, err = f.Git.Head()
		if err != nil {
			return err
		}
		seeded, err := f.Git.CommitMessages(c.base)
		if err != nil {
			return err
		}
		if len(seeded) != 1 || !seeded[0].Empty || seeded[0].Text != c.opts.Title {
			return errors.New("draft seed changed beyond its title-only empty commit")
		}
		ahead = true
	}
	if !ahead {
		return errors.New("branch has no commit ahead of base")
	}
	c.change = collab.Change{Base: c.opts.Base, Head: c.opts.Branch, HeadCommit: c.head}
	var existing collab.ProposalListResult
	if err := f.call(ctx, collab.ContractProposals, collab.OperationFind, collab.ProposalFindInput{Change: c.change, States: []collab.ProposalState{collab.ProposalStateDraft, collab.ProposalStateOpen}}, &existing); err != nil {
		return err
	}
	if existing.Page.Next != "" {
		return errors.New("proposals.find returned another page; exact proposal identity is ambiguous")
	}
	if len(existing.Items) > 1 {
		return errors.New("multiple open proposals for the same base/head")
	}
	if len(existing.Items) == 0 {
		return nil
	}
	if existing.Items[0].Change.Base != c.opts.Base || existing.Items[0].Change.Head != c.opts.Branch {
		return errors.New("proposals.find returned another base/head")
	}
	if precondition(c.proposalCaps, collab.OperationUpsert) {
		c.expectedRevision = existing.Items[0].Revision
		if c.expectedRevision == "" {
			return errors.New("existing proposal lacks revision for upsert precondition")
		}
	}
	return nil
}

func (f Finalizer) publish(ctx context.Context, c *publicationCandidate) (Result, error) {
	if _, err := c.recheck(ctx, f); err != nil {
		return Result{}, err
	}
	if err := f.Git.Push(c.opts.Branch); err != nil {
		return Result{}, fmt.Errorf("push may have happened; inspect origin before resuming: %w", err)
	}
	if _, err := c.recheck(ctx, f); err != nil {
		return Result{}, err
	}
	draft := c.opts.Draft || c.policy.Publication.Draft
	if c.opts.Gate == "ci" {
		draft = false
	}
	request := collab.ProposalUpsertInput{Change: c.change, Title: c.opts.Title, Body: c.body, Draft: &draft, ExpectedRevision: c.expectedRevision}
	result, err := f.upsertAndReadback(ctx, c, request, draft)
	if err != nil {
		return result, err
	}
	if err := f.publicationEvidence(ctx, c, request, &result); err != nil {
		return result, err
	}
	if c.opts.Draft {
		return result, nil
	}
	if err := f.reviewProof(ctx, c, result); err != nil {
		return result, err
	}
	if c.opts.TaskID != "" && !c.opts.PartialDelivery {
		if err := f.transitionTask(ctx, c, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (f Finalizer) upsertAndReadback(ctx context.Context, c *publicationCandidate, request collab.ProposalUpsertInput, draft bool) (Result, error) {
	var upsert collab.ProposalUpsertResult
	if err := f.call(ctx, collab.ContractProposals, collab.OperationUpsert, request, &upsert); err != nil {
		return Result{}, err
	}
	if err := validateProposal(upsert.Proposal, c.change, draft, c.opts.Title, c.body); err != nil {
		return Result{}, err
	}
	var status collab.ProposalStatusResult
	if err := f.call(ctx, collab.ContractProposals, collab.OperationStatus, collab.ProposalRefInput{Ref: upsert.Proposal.Ref}, &status); err != nil {
		return Result{}, err
	}
	if err := validateProposal(status.Proposal, c.change, draft, c.opts.Title, c.body); err != nil {
		return Result{}, err
	}
	if status.Proposal.Ref != upsert.Proposal.Ref {
		return Result{}, errors.New("proposal status returned another reference")
	}
	if err := compareMembers(upsert.Proposal, status.Proposal); err != nil {
		return Result{}, err
	}
	return Result{ProposalRef: status.Proposal.Ref, ProposalURL: status.Proposal.URL, HeadSHA: c.head, State: status.Proposal.State,
		LabelsVerified:    upsert.Proposal.Labels != nil && status.Proposal.Labels != nil,
		AssigneesVerified: upsert.Proposal.Assignees != nil && status.Proposal.Assignees != nil}, nil
}

func (f Finalizer) publicationEvidence(ctx context.Context, c *publicationCandidate, request collab.ProposalUpsertInput, result *Result) error {
	if c.opts.Gate == "ci" {
		if err := f.waitHosted(ctx, c.opts, c.policy, request, result.ProposalRef, c.head, c.originalBody, c.fingerprint, c.base, c.baseTip, c.baseSHA); err != nil {
			return err
		}
		result.Gate = "hosted checks passed on " + c.head
	} else if !c.opts.Draft {
		proof, err := c.recheck(ctx, f)
		if err != nil {
			return err
		}
		result.Gate = "dossier " + c.opts.VerificationFile + ", session " + proof.GateSession
	}
	return nil
}

func (f Finalizer) reviewProof(ctx context.Context, c *publicationCandidate, result Result) error {
	if !operationSupported(c.proposalCaps, collab.OperationReview) {
		return nil
	}
	if _, err := c.recheck(ctx, f); err != nil {
		return err
	}
	comment := fmt.Sprintf("Verification of %s\n\n- ordinary gate: %s\n- local proof: %s", c.head, result.Gate, c.opts.Proof)
	digest := sha256.Sum256([]byte(comment))
	key := "verification:" + c.head + ":" + hex.EncodeToString(digest[:])[:12]
	var review collab.ProposalReviewResult
	return f.call(ctx, collab.ContractProposals, collab.OperationReview, collab.ProposalReviewInput{Ref: result.ProposalRef, Verdict: collab.ReviewVerdictComment, Body: comment, Commit: c.head, IdempotencyKey: key}, &review)
}

func (f Finalizer) transitionTask(ctx context.Context, c *publicationCandidate, result Result) error {
	if _, err := c.recheck(ctx, f); err != nil {
		return err
	}
	var current collab.TaskResult
	if err := f.call(ctx, collab.ContractTasks, collab.OperationGet, collab.TaskRefInput{Ref: c.task.Ref}, &current); err != nil {
		return err
	}
	if current.Task.Ref != c.task.Ref {
		return errors.New("tasks.get changed identity")
	}
	reason := "Proposal " + result.ProposalURL + " published for review"
	if result.ProposalURL == "" {
		reason = "Proposal " + result.ProposalRef.Source + "/" + result.ProposalRef.ID + " published for review"
	}
	if len(reason) > 256 {
		reason = reason[:256]
	}
	transition := collab.TaskTransitionInput{Ref: c.task.Ref, State: c.policy.States.Delivered, Reason: reason}
	if precondition(c.taskCaps, collab.OperationTransition) {
		transition.ExpectedRevision = current.Task.Revision
		if transition.ExpectedRevision == "" {
			return errors.New("task lacks revision for transition precondition")
		}
	}
	if _, err := c.recheck(ctx, f); err != nil {
		return err
	}
	var changed collab.TaskResult
	if err := f.call(ctx, collab.ContractTasks, collab.OperationTransition, transition, &changed); err != nil {
		return err
	}
	if err := f.call(ctx, collab.ContractTasks, collab.OperationGet, collab.TaskRefInput{Ref: c.task.Ref}, &current); err != nil {
		return err
	}
	if current.Task.Ref != c.task.Ref || current.Task.State != c.policy.States.Delivered {
		return errors.New("task state readback failed")
	}
	return nil
}
