package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
)

// HostedWorkflowRequest checks a frozen local workflow at a finalizer boundary.
// Status is nil before publication. A non-nil status must be the unchanged
// native proposals.status result read after the hosted checks completed.
type HostedWorkflowRequest struct {
	File, SHA256                  string
	BaseSHA, HeadSHA, Fingerprint string
	ProposalRef                   collab.Ref
	RequiredChecks                []string
	Status                        *collab.ProposalStatusResult
}

// HostedWorkflowResult separates a possible transition from one actually
// supported by a current native provider result. ReadyUnits contains only
// current candidate units whose native evidence can reach ready.
type HostedWorkflowResult struct {
	WorkflowSHA256          string
	WouldCompleteAfterReady bool
	OutcomeComplete         bool
	ReadyUnits              []string
	IncompleteReason        string
}

// CheckHostedWorkflow is the finalizer's read-only local workflow check. It
// never persists a promoted stage or treats an authored status as hosted proof.
// The finalizer must call it again at each mutation boundary and supply fresh
// native proposal status before relying on OutcomeComplete.
func (v Verifier) CheckHostedWorkflow(ctx context.Context, request HostedWorkflowRequest) (result HostedWorkflowResult, err error) {
	defer recoverFailure(&err)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	require(request.File != "", "invalid frozen workflow reference")
	require(request.SHA256 == "" && request.Status == nil || fingerprintPattern.MatchString(request.SHA256),
		"invalid frozen workflow reference")
	require(baseSHAPattern.MatchString(request.BaseSHA) && baseSHAPattern.MatchString(request.HeadSHA) &&
		fingerprintPattern.MatchString(request.Fingerprint), "invalid hosted workflow binding")
	require(len(request.RequiredChecks) > 0 && !hasDuplicate(stringList(request.RequiredChecks)),
		"hosted workflow policy has no unique named checks")
	for _, name := range request.RequiredChecks {
		require(name != "", "hosted workflow policy has an empty check name")
	}
	top, err := capture("git", gitCommand(v.Dir, "rev-parse", "--show-toplevel"))
	if err != nil {
		return result, err
	}
	r := &verifyRun{v: v, root: jsTrim(top)}
	require(r.git("rev-parse", "HEAD") == request.HeadSHA, "hosted workflow head changed")
	require(r.git("merge-base", request.BaseSHA, "HEAD") == request.BaseSHA, "hosted workflow base changed")
	require(r.fingerprint() == request.Fingerprint, "hosted workflow tree changed")
	var before []byte
	digest := request.SHA256
	if digest == "" {
		before = r.workflowReadPath(request.File, "hosted workflow record")
		sum := sha256.Sum256(before)
		digest = hex.EncodeToString(sum[:])
		r.checked = append(r.checked, checkedRef{path: request.File, digest: digest, workflow: true})
	} else {
		ref := obj([]string{"path", "sha256"}, request.File, digest)
		before, _ = r.workflowBytes(ref, "hosted workflow record")
	}
	workflow, parseErr := parseJSON(before)
	if parseErr != nil {
		return result, parseErr
	}
	// Planning validation checks the complete v1 shape and every native input
	// even when the requested outcome has a later or blocked delivery unit.
	r.verifyWorkflow(workflow, false, request.BaseSHA)
	ready, incomplete := r.hostedReadyProjection(workflow, request.BaseSHA)
	result.WorkflowSHA256 = digest
	result.IncompleteReason = incomplete
	if ready != nil {
		result.WouldCompleteAfterReady = strictEqual(ready.get("outcomeComplete"), true)
		result.ReadyUnits = hostedReadyUnits(workflow, ready)
	}
	if request.Status != nil {
		hostedWorkflowStatus(request, request.Status)
		result.OutcomeComplete = result.WouldCompleteAfterReady
	} else {
		require(request.ProposalRef == (collab.Ref{}), "hosted workflow preflight has a proposal reference")
	}
	require(bytes.Equal(r.workflowReadPath(request.File, "hosted workflow record"), before), "workflow changed during hosted verification")
	r.checkEvidenceStable()
	require(r.git("rev-parse", "HEAD") == request.HeadSHA && r.fingerprint() == request.Fingerprint,
		"hosted workflow tree changed during verification")
	if err := ctx.Err(); err != nil {
		return HostedWorkflowResult{}, err
	}
	return result, nil
}

// A strict ready attempt may fail because a current unit is still incomplete.
// Planning validation already checked structural correctness; a failed ready
// attempt is preserved as a specific blocker for partial delivery.
func (r *verifyRun) hostedReadyProjection(workflow any, baseSHA string) (ready *jsObject, reason string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			switch failure := recovered.(type) {
			case verifyFailure:
				reason = failure.reason
			default:
				panic(recovered)
			}
		}
	}()
	return r.verifyWorkflow(workflow, true, baseSHA), ""
}

func hostedReadyUnits(workflow any, projection *jsObject) []string {
	authored := map[string]string{}
	for _, item := range workflowList(at(workflow, "units"), "workflow units") {
		id := workflowText(at(item, "id"), "workflow unit ID")
		authored[id] = workflowText(at(item, "stage"), "workflow unit stage")
	}
	var ready []string
	for _, item := range workflowList(projection.get("units"), "workflow unit projections") {
		id := workflowText(at(item, "id"), "workflow unit ID")
		if authored[id] != "candidate" || !strictEqual(at(item, "stage"), "ready") ||
			!strictEqual(at(item, "requiredStage"), "ready") || !strictEqual(at(item, "deliveryReady"), true) {
			continue
		}
		for _, member := range workflowList(at(item, "requirements"), "workflow projected requirements") {
			if strictEqual(at(member, "disposition"), "current") {
				ready = append(ready, id)
				break
			}
		}
	}
	slices.Sort(ready)
	return ready
}

func hostedWorkflowStatus(request HostedWorkflowRequest, status *collab.ProposalStatusResult) {
	require(!diag.HasErrors(collab.ValidateResult(status)), "invalid native hosted proposal status")
	require(request.ProposalRef != (collab.Ref{}) && status.Proposal.Ref == request.ProposalRef,
		"hosted workflow status belongs to another proposal")
	require(status.Proposal.State == collab.ProposalStateOpen, "hosted workflow proposal is not ready")
	require(status.Proposal.Change.HeadCommit == request.HeadSHA &&
		status.Checks.Commit == request.HeadSHA, "hosted workflow status names another head")
	for _, name := range request.RequiredChecks {
		found := 0
		for _, check := range status.Checks.Items {
			if check.Name != name {
				continue
			}
			found++
			require(check.State == collab.CheckStatePassing, "hosted workflow check is not passing: "+name)
		}
		require(found == 1, fmt.Sprintf("hosted workflow check %q has %d current results", name, found))
	}
}

// hostedWorkflowReadyReceipt replays a finalizer-produced hosted receipt
// against the original immutable workflow and today's tree. A later workflow
// revision can refer to that result only while the original file still has
// its exact bytes and its native evidence remains current.
func (r *verifyRun) hostedWorkflowReadyReceipt(unit, output *jsObject, expectedBaseSHA string) bool {
	finalizer := workflowObject(output, "workflow hosted finalizer result",
		[]string{"proposalRef", "headSHA", "state", "gate", "workflowSHA256", "readyUnits", "hostedWorkflowReceipt", "labelsVerified", "assigneesVerified"},
		[]string{"proposalRef", "proposalUrl", "headSHA", "state", "gate", "workflowSHA256", "readyUnits", "hostedWorkflowReceipt", "labelsVerified", "assigneesVerified"})
	hosted := workflowObject(finalizer.get("hostedWorkflowReceipt"), "workflow hosted ready receipt",
		[]string{"workflow", "baseSHA", "headSHA", "fingerprint", "proposalRef", "requiredChecks", "status"},
		[]string{"workflow", "baseSHA", "headSHA", "fingerprint", "proposalRef", "requiredChecks", "status"})
	workflow := workflowObject(hosted.get("workflow"), "workflow hosted original", []string{"path", "sha256"}, []string{"path", "sha256"})
	originalDigest := workflowText(workflow.get("sha256"), "workflow hosted original digest")
	r.workflowCheckRef(workflow)
	baseSHA := workflowText(hosted.get("baseSHA"), "workflow hosted base")
	if expectedBaseSHA != "" {
		require(baseSHA == expectedBaseSHA, "workflow hosted receipt names another dossier base")
	}
	headSHA := workflowText(hosted.get("headSHA"), "workflow hosted head")
	fingerprint := workflowText(hosted.get("fingerprint"), "workflow hosted fingerprint")
	proposalRef := workflowProviderRef(hosted.get("proposalRef"), "workflow hosted proposal")
	require(strictEqual(finalizer.get("workflowSHA256"), originalDigest) &&
		strictEqual(finalizer.get("headSHA"), headSHA) &&
		workflowProviderRef(finalizer.get("proposalRef"), "workflow hosted result proposal") == proposalRef &&
		strictEqual(finalizer.get("state"), string(collab.ProposalStateOpen)) &&
		strictEqual(finalizer.get("gate"), "hosted checks passed on "+headSHA),
		"workflow hosted result does not match its native receipt")
	status, diagnostics := collab.ParseResult(collab.ContractProposals, 1, collab.OperationStatus,
		[]byte(stringify(hosted.get("status"), "")))
	if len(diagnostics) > 0 {
		workflowNativeError("hosted proposal status", diagnostics)
	}
	nativeStatus, ok := status.(*collab.ProposalStatusResult)
	require(ok, "workflow hosted receipt has wrong native status")
	request := HostedWorkflowRequest{
		File: workflowText(workflow.get("path"), "workflow hosted original path"), SHA256: originalDigest,
		BaseSHA: baseSHA, HeadSHA: headSHA, Fingerprint: fingerprint,
		ProposalRef: proposalRef, RequiredChecks: workflowStrings(hosted.get("requiredChecks"), "workflow hosted required checks"),
		Status: nativeStatus,
	}
	verifier := r.v
	verifier.Dir = r.root
	replayed, err := verifier.CheckHostedWorkflow(context.Background(), request)
	if err != nil {
		fail("workflow hosted receipt replay failed: " + err.Error())
	}
	require(replayed.WorkflowSHA256 == originalDigest, "workflow hosted receipt changed original digest")
	declared := workflowStrings(finalizer.get("readyUnits"), "workflow hosted ready units")
	slices.Sort(declared)
	require(slices.Equal(declared, replayed.ReadyUnits), "workflow hosted result ready units differ from native replay")
	require(slices.Contains(replayed.ReadyUnits, workflowText(unit.get("id"), "workflow unit ID")),
		"workflow hosted receipt does not cover this unit")
	return true
}
