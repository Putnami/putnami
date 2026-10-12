package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

type Options struct {
	Base             string
	Branch           string
	Title            string
	BodyFile         string
	TaskSource       string
	TaskID           string
	PartialDelivery  bool
	Draft            bool
	VerificationFile string
	WorkflowFile     string
	Gate             string
	Proof            string
}

type Verified struct {
	Fingerprint     string `json:"fingerprint"`
	HeadSHA         string `json:"headSHA"`
	BaseSHA         string `json:"baseSHA"`
	GateSession     string `json:"gateSession"`
	WorkflowPresent bool   `json:"workflowPresent"`
	OutcomeComplete bool   `json:"outcomeComplete"`
}

type Verify func(ctx context.Context, file, base string) (Verified, error)
type Provider func(ctx context.Context, contract, operation string, input json.RawMessage) collab.Envelope

// HostedWorkflow validates a native workflow projection for the current
// candidate. Status is nil during read-only preflight, and is a provider
// status read after hosted checks pass when establishing current ready units.
type HostedWorkflowRequest struct {
	File, SHA256, BaseSHA, HeadSHA, Fingerprint string
	RequiredChecks                              []string
	ProposalRef                                 collab.Ref
	Status                                      *collab.ProposalStatusResult
}
type HostedWorkflowResult struct {
	WorkflowSHA256                           string
	WouldCompleteAfterReady, OutcomeComplete bool
	ReadyUnits                               []string
}
type HostedWorkflow func(context.Context, HostedWorkflowRequest) (HostedWorkflowResult, error)

// HostedWorkflowReceipt records the native provider status and local inputs
// that can be checked again against the unchanged workflow file. It does not
// claim readiness without a fresh native verification of those inputs.
type HostedWorkflowReceipt struct {
	Workflow struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"workflow"`
	BaseSHA        string                      `json:"baseSHA"`
	HeadSHA        string                      `json:"headSHA"`
	Fingerprint    string                      `json:"fingerprint"`
	ProposalRef    collab.Ref                  `json:"proposalRef"`
	RequiredChecks []string                    `json:"requiredChecks"`
	Status         collab.ProposalStatusResult `json:"status"`
}

type Finalizer struct {
	Root     string
	Git      Git
	Verify   Verify
	Workflow HostedWorkflow
	Call     Provider
	Wait     func(context.Context, time.Duration) error
	Now      func() time.Time
}

type Result struct {
	ProposalRef           collab.Ref             `json:"proposalRef"`
	ProposalURL           string                 `json:"proposalUrl,omitempty"`
	HeadSHA               string                 `json:"headSHA"`
	State                 collab.ProposalState   `json:"state"`
	Gate                  string                 `json:"gate,omitempty"`
	WorkflowSHA256        string                 `json:"workflowSHA256,omitempty"`
	ReadyUnits            []string               `json:"readyUnits,omitempty"`
	HostedWorkflowReceipt *HostedWorkflowReceipt `json:"hostedWorkflowReceipt,omitempty"`
	LabelsVerified        bool                   `json:"labelsVerified"`
	AssigneesVerified     bool                   `json:"assigneesVerified"`
}

func (f Finalizer) call(ctx context.Context, contract, operation string, input any, output any) error {
	if f.Call == nil {
		return errors.New("collaboration provider unavailable")
	}
	request, err := json.Marshal(input)
	if err != nil {
		return err
	}
	envelope := f.Call(ctx, contract, operation, request)
	if envelope.Outcome != collab.OutcomeOK {
		if envelope.Error == nil {
			return fmt.Errorf("%s.%s answered %s", contract, operation, envelope.Outcome)
		}
		if envelope.Outcome == collab.OutcomeUnresolved {
			return fmt.Errorf("%s.%s unresolved: %s; reconcile with %s before resuming; no write was retried", contract, operation, envelope.Error.Message, envelope.Error.Reconcile)
		}
		return fmt.Errorf("%s.%s answered %s (%s): %s", contract, operation, envelope.Outcome, envelope.Error.Reason, envelope.Error.Message)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, output); err != nil {
		return fmt.Errorf("invalid %s.%s result: %w", contract, operation, err)
	}
	return nil
}

func (f Finalizer) capabilities(ctx context.Context, contract string, needed ...string) (collab.Capabilities, error) {
	var answer collab.Capabilities
	if err := f.call(ctx, contract, collab.OperationCapabilities, struct{}{}, &answer); err != nil {
		return answer, err
	}
	if answer.Status != collab.BindingStatusBound {
		return answer, fmt.Errorf("%s binding is %s", contract, answer.Status)
	}
	for _, wanted := range needed {
		found := false
		for _, op := range answer.Operations {
			if op.Name == wanted && op.Supported {
				found = true
				break
			}
		}
		if !found {
			return answer, fmt.Errorf("%s provider does not support %s", contract, wanted)
		}
	}
	return answer, nil
}

func precondition(c collab.Capabilities, operation string) bool {
	for _, op := range c.Operations {
		if op.Name == operation {
			return op.Preconditions == collab.PreconditionsAtomic || op.Preconditions == collab.PreconditionsChecked
		}
	}
	return false
}

func (f Finalizer) verify(ctx context.Context, opts Options, fingerprint, head, baseSHA, baseRef string) (Verified, error) {
	if opts.Draft || opts.Gate == "ci" {
		return Verified{}, nil
	}
	if f.Verify == nil {
		return Verified{}, errors.New("native dossier verifier unavailable")
	}
	result, err := f.Verify(ctx, opts.VerificationFile, baseRef)
	if err != nil {
		return result, err
	}
	if result.Fingerprint != fingerprint || result.HeadSHA != head || result.BaseSHA != baseSHA || result.GateSession == "" {
		return result, errors.New("verified dossier binding does not match the committed candidate")
	}
	if result.WorkflowPresent && !result.OutcomeComplete && opts.TaskID != "" && !opts.PartialDelivery {
		return result, errors.New("workflow outcome is incomplete; publish the intermediate unit with --partial-delivery")
	}
	return result, nil
}

func (f Finalizer) frozen(ctx context.Context, opts Options, originalBody []byte, head, fingerprint, baseRef, baseTip, baseSHA string) (Verified, error) {
	branch, err := f.Git.Branch()
	if err != nil {
		return Verified{}, err
	}
	if branch != opts.Branch {
		return Verified{}, fmt.Errorf("branch changed: %q", branch)
	}
	actualHead, err := f.Git.Head()
	if err != nil {
		return Verified{}, err
	}
	if actualHead != head {
		return Verified{}, errors.New("HEAD changed during finalization")
	}
	actualTip, err := f.Git.BaseTip(baseRef)
	if err != nil {
		return Verified{}, err
	}
	if actualTip != baseTip {
		return Verified{}, errors.New("publication base moved during finalization")
	}
	actualBaseSHA, err := f.Git.BaseSHA(baseRef)
	if err != nil {
		return Verified{}, err
	}
	if actualBaseSHA != baseSHA {
		return Verified{}, errors.New("publication merge base moved during finalization")
	}
	if !opts.Draft {
		clean, err := f.Git.Clean()
		if err != nil {
			return Verified{}, err
		}
		if !clean {
			return Verified{}, errors.New("ready finalization requires a clean committed tree")
		}
		actualFingerprint, err := f.Git.Fingerprint()
		if err != nil {
			return Verified{}, err
		}
		if actualFingerprint != fingerprint {
			return Verified{}, errors.New("tree changed during finalization")
		}
	}
	currentBody, err := os.ReadFile(opts.BodyFile)
	if err != nil {
		return Verified{}, err
	}
	if string(currentBody) != string(originalBody) {
		return Verified{}, errors.New("body file changed during finalization")
	}
	return f.verify(ctx, opts, fingerprint, head, baseSHA, baseRef)
}

func (f Finalizer) Run(ctx context.Context, opts Options) (Result, error) {
	if f.Git == nil {
		f.Git = NativeGit{Root: f.Root}
	}
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Wait == nil {
		f.Wait = waitContext
	}
	candidate, err := f.prepare(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	if err := f.seedAndFind(ctx, &candidate); err != nil {
		return Result{}, err
	}
	return f.publish(ctx, &candidate)
}

func operationSupported(c collab.Capabilities, name string) bool {
	for _, op := range c.Operations {
		if op.Name == name {
			return op.Supported
		}
	}
	return false
}

func validateProposal(proposal collab.Proposal, change collab.Change, draft bool, title, body string) error {
	if proposal.Ref.Source == "" || proposal.Ref.ID == "" {
		return errors.New("provider returned no proposal reference")
	}
	if proposal.Change.Repository == "" {
		return errors.New("provider returned no proposal repository")
	}
	if proposal.Change.Base != change.Base || proposal.Change.Head != change.Head || proposal.Change.HeadCommit != change.HeadCommit {
		return errors.New("proposal base, head or full head commit readback failed")
	}
	if proposal.Title != title || proposal.Body != body {
		return errors.New("proposal title or body readback failed")
	}
	want := collab.ProposalStateOpen
	if draft {
		want = collab.ProposalStateDraft
	}
	if proposal.State != want {
		return fmt.Errorf("proposal state is %q, expected %q", proposal.State, want)
	}
	return nil
}

func compareMembers(upsert, read collab.Proposal) error {
	if upsert.Change.Repository != read.Change.Repository {
		return errors.New("proposal repository changed between upsert and readback")
	}
	for name, pair := range map[string][2][]string{"labels": {upsert.Labels, read.Labels}, "assignees": {upsert.Assignees, read.Assignees}} {
		if pair[0] != nil && pair[1] == nil {
			return fmt.Errorf("proposal readback dropped %s", name)
		}
		if pair[0] == nil || pair[1] == nil {
			continue
		}
		members := map[string]bool{}
		for _, value := range pair[1] {
			members[value] = true
		}
		for _, value := range pair[0] {
			if !members[value] {
				return fmt.Errorf("proposal readback omitted %s %q", name, value)
			}
		}
	}
	return nil
}

func waitContext(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f Finalizer) waitHosted(ctx context.Context, c *publicationCandidate, request collab.ProposalUpsertInput, ref collab.Ref) (collab.ProposalStatusResult, error) {
	deadline := f.Now().Add(time.Duration(c.policy.Verification.CIGate.TimeoutMinutes) * time.Minute)
	for {
		if _, err := c.recheck(ctx, f); err != nil {
			return collab.ProposalStatusResult{}, err
		}
		var status collab.ProposalStatusResult
		if err := f.call(ctx, collab.ContractProposals, collab.OperationStatus, collab.ProposalRefInput{Ref: ref}, &status); err != nil {
			return status, err
		}
		if status.Proposal.Ref != ref || status.Proposal.Change.HeadCommit != c.head || status.Proposal.State != collab.ProposalStateOpen {
			return status, errors.New("hosted check proposal identity, head or open state changed")
		}
		verdict := hostedCheckVerdict(status.Checks, c.policy.Verification.CIGate.Checks, c.head)
		switch verdict {
		case "passing":
			if _, err := c.recheck(ctx, f); err != nil {
				return status, err
			}
			return status, nil
		case "failing", "unsupported":
			if _, err := c.recheck(ctx, f); err != nil {
				return status, err
			}
			request.Draft = new(bool)
			*request.Draft = true
			request.ExpectedRevision = status.Proposal.Revision
			if request.ExpectedRevision == "" {
				return status, errors.New("proposal lacks revision for return-to-draft precondition")
			}
			var returned collab.ProposalUpsertResult
			if err := f.call(ctx, collab.ContractProposals, collab.OperationUpsert, request, &returned); err != nil {
				return status, fmt.Errorf("hosted checks %s; return to draft: %w", verdict, err)
			}
			if err := validateProposal(returned.Proposal, request.Change, true, request.Title, request.Body); err != nil {
				return status, err
			}
			var readback collab.ProposalStatusResult
			if err := f.call(ctx, collab.ContractProposals, collab.OperationStatus, collab.ProposalRefInput{Ref: ref}, &readback); err != nil {
				return status, err
			}
			if err := validateProposal(readback.Proposal, request.Change, true, request.Title, request.Body); err != nil {
				return status, err
			}
			return status, fmt.Errorf("hosted checks %s; proposal returned to draft", verdict)
		}
		if !f.Now().Before(deadline) {
			return status, errors.New("hosted checks pending at timeout; resume finalization to continue waiting")
		}
		if err := f.Wait(ctx, time.Duration(c.policy.Verification.CIGate.PollSeconds)*time.Second); err != nil {
			return status, err
		}
	}
}

func hostedCheckVerdict(checks collab.Checks, names []string, head string) string {
	if checks.State == collab.ChecksStateUnsupported {
		return "unsupported"
	}
	if len(names) == 0 {
		return "unsupported"
	}
	if checks.Commit != head {
		return "pending"
	}
	allPassing := true
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			return "unsupported"
		}
		seen[name] = true
		count := 0
		var state collab.CheckState
		for _, check := range checks.Items {
			if check.Name == name {
				count++
				state = check.State
			}
		}
		if count > 1 {
			return "failing"
		}
		if count == 0 {
			allPassing = false
			continue
		}
		if state == collab.CheckStateFailing {
			return "failing"
		}
		if state != collab.CheckStatePassing {
			allPassing = false
		}
	}
	if allPassing {
		return "passing"
	}
	return "pending"
}
