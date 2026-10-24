package finalize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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
	existingProposal        *collab.Proposal
	expectedRevision        string
	workflowSHA             string
	readyUnits              []string
}

func (c *publicationCandidate) recheck(ctx context.Context, f Finalizer) (Verified, error) {
	verified, err := f.frozen(ctx, c.opts, c.originalBody, c.head, c.fingerprint, c.base, c.baseTip, c.baseSHA)
	if err != nil {
		return verified, err
	}
	if c.opts.Gate != "ci" || c.opts.TaskID == "" || c.opts.WorkflowFile == "" {
		return verified, nil
	}
	if f.Workflow == nil {
		return verified, errors.New("native hosted workflow verifier unavailable")
	}
	projection, err := f.Workflow(ctx, c.workflowRequest(nil))
	if err != nil {
		return verified, err
	}
	if projection.WorkflowSHA256 == "" || c.workflowSHA != "" && projection.WorkflowSHA256 != c.workflowSHA {
		return verified, errors.New("hosted workflow changed during finalization")
	}
	if !c.opts.PartialDelivery && !projection.WouldCompleteAfterReady {
		return verified, errors.New("hosted workflow is not eligible to complete the parent; use --partial-delivery")
	}
	if c.opts.PartialDelivery && len(projection.ReadyUnits) == 0 {
		return verified, errors.New("hosted workflow has no eligible current unit for partial delivery")
	}
	c.workflowSHA = projection.WorkflowSHA256
	return verified, nil
}

func (c *publicationCandidate) workflowRequest(status *collab.ProposalStatusResult) HostedWorkflowRequest {
	return HostedWorkflowRequest{File: c.opts.WorkflowFile, SHA256: c.workflowSHA, BaseSHA: c.baseSHA,
		HeadSHA: c.head, Fingerprint: c.fingerprint, RequiredChecks: c.policy.Verification.CIGate.Checks, Status: status}
}

func (c *publicationCandidate) promoteHosted(ctx context.Context, f Finalizer, status *collab.ProposalStatusResult) error {
	if c.opts.TaskID == "" || c.opts.WorkflowFile == "" {
		return nil
	}
	if _, err := c.recheck(ctx, f); err != nil {
		return err
	}
	request := c.workflowRequest(status)
	request.ProposalRef = status.Proposal.Ref
	projection, err := f.Workflow(ctx, request)
	if err != nil {
		return err
	}
	if projection.WorkflowSHA256 != c.workflowSHA {
		return errors.New("hosted workflow changed before ready promotion")
	}
	if !c.opts.PartialDelivery && !projection.OutcomeComplete {
		return errors.New("hosted workflow remains incomplete; parent task was not transitioned")
	}
	if c.opts.PartialDelivery && len(projection.ReadyUnits) == 0 {
		return errors.New("hosted workflow has no eligible current unit for partial delivery")
	}
	c.readyUnits = projection.ReadyUnits
	return nil
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
	if opts.WorkflowFile != "" && opts.Gate != "ci" {
		return Policy{}, errors.New("--workflow-file requires --gate ci")
	}
	if opts.Gate == "ci" && opts.TaskID != "" && opts.WorkflowFile == "" && !opts.PartialDelivery {
		return Policy{}, errors.New("--gate ci with a parent task requires --workflow-file")
	}
	if !opts.Draft && (opts.VerificationFile == "") == (opts.Gate != "ci") {
		return Policy{}, errors.New("ready publication needs exactly one of --verification-file or --gate ci")
	}
	policy, err := ReadPolicy(f.Root)
	if err != nil {
		return policy, err
	}
	if opts.Gate == "ci" {
		if len(policy.Verification.CIGate.Checks) == 0 || policy.Publication.Draft {
			return policy, errors.New("--gate ci requires named hosted checks and a ready publication policy")
		}
		seen := make(map[string]bool, len(policy.Verification.CIGate.Checks))
		for _, name := range policy.Verification.CIGate.Checks {
			if name == "" || seen[name] {
				return policy, errors.New("--gate ci requires distinct nonempty hosted check names")
			}
			seen[name] = true
		}
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

// checkFeaturelessPartial admits a hosted partial delivery without a workflow
// record only when every changed path belongs to projects that own and author
// no feature. The hosted checks on the bound head are then its evidence.
func (f Finalizer) checkFeaturelessPartial(ctx context.Context, c *publicationCandidate) error {
	if c.opts.Gate != "ci" || c.opts.TaskID == "" || c.opts.WorkflowFile != "" {
		return nil
	}
	if f.Owners == nil {
		return errors.New("native project ownership unavailable")
	}
	paths, err := f.Git.ChangedPaths(c.base)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("hosted partial delivery without --workflow-file has no changed path")
	}
	owners, err := f.Owners(ctx, c.baseSHA, paths)
	if err != nil {
		return err
	}
	var reasons []string
	if len(owners.Projects) > 0 {
		reasons = append(reasons, "projects without featureAuthority.none or with authored features: "+boundedList(owners.Projects))
	}
	if len(owners.Unowned) > 0 {
		reasons = append(reasons, "paths no project owns: "+boundedList(owners.Unowned))
	}
	if len(reasons) > 0 {
		return fmt.Errorf("--gate ci with a parent task requires --workflow-file; %s", strings.Join(reasons, "; "))
	}
	return nil
}

func boundedList(items []string) string {
	const shown = 10
	if len(items) <= shown {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:shown], ", "), len(items)-shown)
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
	if err := f.checkFeaturelessPartial(ctx, c); err != nil {
		return err
	}
	_, err = c.recheck(ctx, f)
	return err
}

// seedAndFind looks up the branch's proposals before it seeds or pushes. A
// result answers the run without a write: the branch's proposal is already
// merged. An error stops the run when the branch's proposal is closed or
// merged at another head.
func (f Finalizer) seedAndFind(ctx context.Context, c *publicationCandidate) (*Result, error) {
	c.change = collab.Change{Base: c.opts.Base, Head: c.opts.Branch, HeadCommit: c.head}
	existing, settled, err := f.findProposal(ctx, c)
	if err != nil {
		return nil, err
	}
	if existing == nil && len(settled) > 0 {
		return settledOutcome(c, settled)
	}
	ahead, err := f.Git.Ahead(c.base)
	if err != nil {
		return nil, err
	}
	if c.opts.Draft && !ahead {
		if err := f.Git.Seed(c.opts.Title); err != nil {
			return nil, err
		}
		c.head, err = f.Git.Head()
		if err != nil {
			return nil, err
		}
		seeded, err := f.Git.CommitMessages(c.base)
		if err != nil {
			return nil, err
		}
		if len(seeded) != 1 || !seeded[0].Empty || seeded[0].Text != c.opts.Title {
			return nil, errors.New("draft seed changed beyond its title-only empty commit")
		}
		c.change.HeadCommit = c.head
		ahead = true
	}
	if !ahead {
		return nil, errors.New("branch has no commit ahead of base")
	}
	c.existingProposal = existing
	if existing == nil || !precondition(c.proposalCaps, collab.OperationUpsert) {
		return nil, nil
	}
	c.expectedRevision = existing.Revision
	if c.expectedRevision == "" {
		return nil, errors.New("existing proposal lacks revision for upsert precondition")
	}
	return nil, nil
}

// settledOutcome decides a branch that has merged or closed proposals and no
// open one. A merged proposal at the current head means the work is already
// delivered. Any other settled proposal stops the run: opening a replacement
// would publish the same change again or work its reviewers already refused.
func settledOutcome(c *publicationCandidate, settled []collab.Proposal) (*Result, error) {
	for _, proposal := range settled {
		if proposal.State == collab.ProposalStateMerged && proposal.Change.HeadCommit == c.head {
			return &Result{ProposalRef: proposal.Ref, ProposalURL: proposal.URL, HeadSHA: c.head, State: proposal.State}, nil
		}
	}
	names := make([]string, 0, len(settled))
	for _, proposal := range settled {
		name := fmt.Sprintf("%s %s/%s", proposal.State, proposal.Ref.Source, proposal.Ref.ID)
		if proposal.URL != "" {
			name += " (" + proposal.URL + ")"
		}
		if proposal.State == collab.ProposalStateMerged {
			name += " at " + proposal.Change.HeadCommit
		}
		names = append(names, name)
	}
	return nil, fmt.Errorf("branch %q has no open proposal and already has %s, while HEAD is %s; finalize opens no replacement: reopen the closed proposal, or publish the new commits from another branch",
		c.opts.Branch, boundedList(names), c.head)
}

// maxFindPages bounds the pages of one proposals.find traversal.
const maxFindPages = 10

// findProposal reads every proposal of the change in every state. It returns
// the one open or draft proposal, if any, and the merged and closed ones.
func (f Finalizer) findProposal(ctx context.Context, c *publicationCandidate) (*collab.Proposal, []collab.Proposal, error) {
	var found []collab.Proposal
	var page *collab.PageRequest
	for pages := 0; ; pages++ {
		var result collab.ProposalListResult
		if err := f.call(ctx, collab.ContractProposals, collab.OperationFind, collab.ProposalFindInput{Change: c.change, Page: page}, &result); err != nil {
			return nil, nil, err
		}
		found = append(found, result.Items...)
		if result.Page.Next == "" {
			break
		}
		if pages+1 == maxFindPages {
			return nil, nil, errors.New("proposals.find returned too many pages; exact proposal identity is ambiguous")
		}
		page = &collab.PageRequest{Cursor: result.Page.Next}
	}
	var open *collab.Proposal
	var settled []collab.Proposal
	for i, proposal := range found {
		if proposal.Ref.Source == "" || proposal.Ref.ID == "" || proposal.Change.Repository == "" ||
			proposal.Change.Base != c.opts.Base || proposal.Change.Head != c.opts.Branch {
			return nil, nil, errors.New("proposals.find returned an incomplete or different proposal identity/base/head")
		}
		switch proposal.State {
		case collab.ProposalStateOpen, collab.ProposalStateDraft:
			if open != nil {
				return nil, nil, errors.New("multiple open proposals for the same base/head")
			}
			open = &found[i]
		case collab.ProposalStateMerged, collab.ProposalStateClosed:
			settled = append(settled, proposal)
		default:
			return nil, nil, fmt.Errorf("proposals.find returned state %q", proposal.State)
		}
	}
	return open, settled, nil
}

func sameMembers(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	left, right := slices.Clone(a), slices.Clone(b)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func sameProposalExceptHead(before, after collab.Proposal) bool {
	// UpdatedAt is provider-maintained display metadata. The proposed content
	// and identity must remain unchanged across the publisher's own push.
	return before.Ref == after.Ref && before.Change.Repository == after.Change.Repository &&
		before.Change.Base == after.Change.Base && before.Change.Head == after.Change.Head &&
		before.Title == after.Title && before.Body == after.Body && before.State == after.State &&
		before.ProviderState == after.ProviderState && sameMembers(before.Labels, after.Labels) &&
		sameMembers(before.Assignees, after.Assignees)
}

func (f Finalizer) refreshAfterPush(ctx context.Context, c *publicationCandidate) error {
	current, _, err := f.findProposal(ctx, c)
	if err != nil {
		return err
	}
	if c.existingProposal == nil {
		if current != nil {
			return errors.New("proposal appeared after push; no proposal write was attempted")
		}
		return nil
	}
	if current == nil || !sameProposalExceptHead(*c.existingProposal, *current) {
		return errors.New("proposal identity or content changed after push; no proposal write was attempted")
	}
	previous := c.existingProposal
	switch {
	case current.Change.HeadCommit == previous.Change.HeadCommit:
		if current.Revision != previous.Revision {
			return errors.New("proposal revision changed without a head change; no proposal write was attempted")
		}
	case current.Change.HeadCommit == c.head:
		if current.Revision == previous.Revision {
			return errors.New("proposal head changed without a new revision; no proposal write was attempted")
		}
	default:
		return errors.New("proposal head changed to a commit other than the pushed full HEAD; no proposal write was attempted")
	}
	if precondition(c.proposalCaps, collab.OperationUpsert) {
		if current.Revision == "" {
			return errors.New("post-push proposal lacks revision for upsert precondition")
		}
		c.expectedRevision = current.Revision
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
	if err := f.refreshAfterPush(ctx, c); err != nil {
		return Result{}, err
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
	if _, err := c.recheck(ctx, f); err != nil {
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
		status, err := f.waitHosted(ctx, c, request, result.ProposalRef)
		if err != nil {
			return err
		}
		if err := c.promoteHosted(ctx, f, &status); err != nil {
			return err
		}
		result.Gate = "hosted checks passed on " + c.head
		result.WorkflowSHA256 = c.workflowSHA
		result.ReadyUnits = c.readyUnits
		if c.opts.TaskID != "" && len(c.readyUnits) > 0 {
			receipt := &HostedWorkflowReceipt{
				BaseSHA: c.baseSHA, HeadSHA: c.head, Fingerprint: c.fingerprint,
				ProposalRef:    result.ProposalRef,
				RequiredChecks: append([]string(nil), c.policy.Verification.CIGate.Checks...),
				Status:         status,
			}
			receipt.Workflow.Path = c.opts.WorkflowFile
			receipt.Workflow.SHA256 = c.workflowSHA
			result.HostedWorkflowReceipt = receipt
		}
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
	if err := f.confirmHostedReady(ctx, c, result.ProposalRef); err != nil {
		return err
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
	reason = truncateUTF8(reason, 256)
	transition := collab.TaskTransitionInput{Ref: c.task.Ref, State: c.policy.States.Delivered, Reason: reason}
	if precondition(c.taskCaps, collab.OperationTransition) {
		transition.ExpectedRevision = current.Task.Revision
		if transition.ExpectedRevision == "" {
			return errors.New("task lacks revision for transition precondition")
		}
	}
	if err := f.confirmHostedReady(ctx, c, result.ProposalRef); err != nil {
		return err
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

func (f Finalizer) confirmHostedReady(ctx context.Context, c *publicationCandidate, ref collab.Ref) error {
	if c.opts.Gate != "ci" {
		return nil
	}
	if _, err := c.recheck(ctx, f); err != nil {
		return err
	}
	var status collab.ProposalStatusResult
	if err := f.call(ctx, collab.ContractProposals, collab.OperationStatus, collab.ProposalRefInput{Ref: ref}, &status); err != nil {
		return err
	}
	if status.Proposal.Ref != ref || status.Proposal.Change.HeadCommit != c.head || status.Proposal.State != collab.ProposalStateOpen ||
		hostedCheckVerdict(status.Checks, c.policy.Verification.CIGate.Checks, c.head) != "passing" {
		return errors.New("hosted proposal or required checks changed before dependent publication")
	}
	if c.opts.TaskID == "" {
		return nil
	}
	return c.promoteHosted(ctx, f, &status)
}

// truncateUTF8 cuts text to at most limit bytes without splitting a character.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
