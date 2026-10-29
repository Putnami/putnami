package verify

import (
	"context"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func hostedWorkflowFixture(t *testing.T) (*verifyFixture, fxObj, HostedWorkflowRequest) {
	t.Helper()
	f, workflow := newWorkflowFixture(t)
	f.write(".context/workflow.json", workflow)
	f.resetFingerprint(fxFingerprint{})
	ref := f.reference(".context/workflow.json")
	digest, ok := ref["sha256"].(string)
	if !ok {
		t.Fatal("workflow fixture has no digest")
	}
	request := HostedWorkflowRequest{
		File: ".context/workflow.json", SHA256: digest,
		BaseSHA: f.head, HeadSHA: f.head, Fingerprint: fxTree,
		RequiredChecks: []string{"hosted-gate"},
	}
	return f, workflow, request
}

func (f *verifyFixture) checkHostedWorkflow(request HostedWorkflowRequest) (HostedWorkflowResult, error) {
	f.t.Helper()
	f.resetFingerprint(fxFingerprint{})
	verifier := Verifier{Dir: f.root, Fingerprint: f.fakeFingerprint, Plan: f.fakePlan, RecoverReport: f.recoverReport}
	return verifier.CheckHostedWorkflow(context.Background(), request)
}

func hostedPassingStatus(head string) *collab.ProposalStatusResult {
	return &collab.ProposalStatusResult{
		Proposal: collab.Proposal{Ref: collab.Ref{Source: "local:fixture", ID: "proposal-1"},
			Revision: "revision-1", Title: "Fixture proposal", State: collab.ProposalStateOpen,
			Change: collab.Change{Repository: "fixture", Base: "main", Head: "feature", HeadCommit: head}},
		Checks: collab.Checks{State: collab.ChecksStatePassing, Commit: head,
			Items: []collab.Check{{Name: "hosted-gate", State: collab.CheckStatePassing}}},
	}
}

func TestHostedWorkflowPromotesOnlyWithNativeStatus(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	f, workflow, request := hostedWorkflowFixture(t)
	wantDigest := request.SHA256
	request.SHA256 = "" // The first read derives the frozen reference before any publication write.
	preflight, err := f.checkHostedWorkflow(request)
	if err != nil || !preflight.WouldCompleteAfterReady || preflight.OutcomeComplete ||
		len(preflight.ReadyUnits) != 1 || preflight.ReadyUnits[0] != "first" || preflight.WorkflowSHA256 != wantDigest {
		t.Fatalf("hosted preflight = %+v, %v", preflight, err)
	}
	progress := f.workflow(workflow, false, "")
	if progress.value["outcomeComplete"] != false {
		t.Fatalf("standalone progress claimed hosted readiness: %s", progress.out)
	}
	request.Status = hostedPassingStatus(request.HeadSHA)
	request.ProposalRef = request.Status.Proposal.Ref
	request.SHA256 = preflight.WorkflowSHA256
	verified, err := f.checkHostedWorkflow(request)
	if err != nil || !verified.OutcomeComplete || !verified.WouldCompleteAfterReady ||
		len(verified.ReadyUnits) != 1 || verified.ReadyUnits[0] != "first" {
		t.Fatalf("hosted status did not prove eligible unit: %+v, %v", verified, err)
	}
	progress = f.workflow(workflow, false, "")
	if progress.value["outcomeComplete"] != false {
		t.Fatalf("hosted API mutated authored workflow: %s", progress.out)
	}
}

func TestHostedWorkflowRejectsStaleAndAmbiguousStatus(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	f, _, request := hostedWorkflowFixture(t)
	request.Status = hostedPassingStatus(request.HeadSHA)
	request.ProposalRef = request.Status.Proposal.Ref
	for _, tc := range []struct {
		name, reason string
		change       func(*HostedWorkflowRequest)
	}{
		{"wrong proposal", "another proposal", func(r *HostedWorkflowRequest) { r.ProposalRef.ID = "other" }},
		{"draft proposal", "not ready", func(r *HostedWorkflowRequest) { r.Status.Proposal.State = collab.ProposalStateDraft }},
		{"short head", "another head", func(r *HostedWorkflowRequest) { r.Status.Proposal.Change.HeadCommit = r.HeadSHA[:7] }},
		{"stale checks", "another head", func(r *HostedWorkflowRequest) { r.Status.Checks.Commit = strings.Repeat("a", 40) }},
		{"missing check", "0 current results", func(r *HostedWorkflowRequest) { r.Status.Checks.Items = nil }},
		{"failed check", "not passing", func(r *HostedWorkflowRequest) { r.Status.Checks.Items[0].State = collab.CheckStateFailing }},
		{"duplicate mixed check", "not passing", func(r *HostedWorkflowRequest) {
			r.Status.Checks.Items = append(r.Status.Checks.Items, collab.Check{Name: "hosted-gate", State: collab.CheckStateFailing})
		}},
		{"duplicate passing check", "2 current results", func(r *HostedWorkflowRequest) {
			r.Status.Checks.Items = append(r.Status.Checks.Items, collab.Check{Name: "hosted-gate", State: collab.CheckStatePassing})
		}},
		{"duplicate policy name", "no unique named checks", func(r *HostedWorkflowRequest) { r.RequiredChecks = []string{"hosted-gate", "hosted-gate"} }},
		{"missing frozen digest", "invalid frozen workflow reference", func(r *HostedWorkflowRequest) { r.SHA256 = "" }},
		{"stale workflow digest", "evidence digest changed", func(r *HostedWorkflowRequest) { r.SHA256 = strings.Repeat("a", 64) }},
		{"stale fingerprint", "tree changed", func(r *HostedWorkflowRequest) { r.Fingerprint = strings.Repeat("b", 64) }},
		{"stale local head", "head changed", func(r *HostedWorkflowRequest) { r.HeadSHA = strings.Repeat("a", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := request
			status := *request.Status
			status.Proposal = request.Status.Proposal
			status.Checks = request.Status.Checks
			status.Checks.Items = append([]collab.Check(nil), request.Status.Checks.Items...)
			copy.Status = &status
			copy.RequiredChecks = append([]string(nil), request.RequiredChecks...)
			tc.change(&copy)
			_, err := f.checkHostedWorkflow(copy)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("hosted status error = %v, want %q", err, tc.reason)
			}
		})
	}
}

func TestHostedWorkflowDoesNotPromoteBlockedCurrentUnit(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		change       func(*verifyFixture, fxObj)
	}{
		{"unresolved decision", "unresolved decisions", func(f *verifyFixture, workflow fxObj) {
			fxObjAt(workflow, "units", 0)["decisions"] = fxArr{f.note("hosted-decision", "Owner decision pending.")}
		}},
		{"blocked stage", "design stage incomplete", func(f *verifyFixture, workflow fxObj) {
			stage := fxObjAt(workflow, "stages", 2)
			stage["state"] = "blocked"
			stage["reason"] = f.note("hosted-blocker", "Design is blocked.")
		}},
		{"missing review", "material workflow artifact lacks independent challenge", func(f *verifyFixture, workflow fxObj) {
			workflow["artifacts"] = fxArr{fxObj{"id": "current-design", "kind": "design", "contractVersion": "1",
				"lifecycle": "proposed", "units": fxArr{"first"}, "scopes": fxArr{"/.context"},
				"source": f.note("hosted-design", "A current material design."), "context": fxArr{},
				"author": "designer", "material": true}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, workflow, request := hostedWorkflowFixture(t)
			tc.change(f, workflow)
			f.write(".context/workflow.json", workflow)
			ref := f.reference(".context/workflow.json")
			digest, ok := ref["sha256"].(string)
			if !ok {
				t.Fatal("workflow fixture has no digest")
			}
			request.SHA256 = digest
			request.Status = hostedPassingStatus(request.HeadSHA)
			request.ProposalRef = request.Status.Proposal.Ref
			result, err := f.checkHostedWorkflow(request)
			if err != nil || result.OutcomeComplete || result.WouldCompleteAfterReady ||
				len(result.ReadyUnits) != 0 || !strings.Contains(result.IncompleteReason, tc.reason) {
				t.Fatalf("blocked hosted unit = %+v, %v; want %q", result, err, tc.reason)
			}
		})
	}
}

func TestHostedChecksNeverStandForNativeProofOfABuildingUnit(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		change       func(fxObj)
	}{
		{"proven building unit", "has not reached candidate: first", func(fxObj) {}},
		{"unproven building unit", "needs native producer proof", func(workflow fxObj) { delete(workflow, "proof") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, workflow, request := hostedWorkflowFixture(t)
			fxObjAt(workflow, "units", 0)["stage"] = "building"
			tc.change(workflow)
			f.write(".context/workflow.json", workflow)
			ref := f.reference(".context/workflow.json")
			digest, ok := ref["sha256"].(string)
			if !ok {
				t.Fatal("workflow fixture has no digest")
			}
			request.SHA256 = digest
			preflight, err := f.checkHostedWorkflow(request)
			if err != nil || preflight.WouldCompleteAfterReady || len(preflight.ReadyUnits) != 0 ||
				!strings.Contains(preflight.IncompleteReason, tc.reason) {
				t.Fatalf("building unit preflight = %+v, %v; want a valid workflow with no ready unit and %q", preflight, err, tc.reason)
			}
			request.Status = hostedPassingStatus(request.HeadSHA)
			request.ProposalRef = request.Status.Proposal.Ref
			hosted, err := f.checkHostedWorkflow(request)
			if err != nil || hosted.OutcomeComplete || hosted.WouldCompleteAfterReady || len(hosted.ReadyUnits) != 0 {
				t.Fatalf("passing hosted checks stood for native proof: %+v, %v", hosted, err)
			}
		})
	}
}

func TestHostedWorkflowPreservesLaterOutcomeScope(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	f, workflow, request := hostedWorkflowFixture(t)
	manifest := f.readJSON(".context/putnami.features.json")
	feature := fxObjAt(manifest, "features", 0)
	feature["requirements"] = append(fxArrAt(feature, "requirements"), fxObj{
		"id": "later", "stage": "coded", "evidenceKinds": fxArr{"attestation"},
		"verification": fxObj{"kind": "acceptance", "checks": fxArr{"later-check"}},
	})
	f.write(".context/putnami.features.json", manifest)
	spec := f.readJSON(".context/specs/workflow.json")
	spec["requirements"] = append(fxArrAt(spec, "requirements"), fxObj{"id": "later", "text": "The later outcome remains requested."})
	f.write(".context/specs/workflow.json", spec)
	source := fxObjAt(workflow, "sources", 0)
	source["manifest"] = f.reference(".context/putnami.features.json")
	source["spec"] = f.reference(".context/specs/workflow.json")
	workflow["requirements"] = append(fxArrAt(workflow, "requirements"), fxObj{
		"feature": workflowFeature, "id": "later", "disposition": "later", "unit": "later",
	})
	workflow["units"] = append(fxArrAt(workflow, "units"), fxObj{
		"id": "later", "outcome": "Later capability", "boundary": f.note("hosted-later-boundary", "Later outcome remains in scope."),
		"exclusions": fxArr{}, "projects": fxArr{fxObj{"id": "/.context", "manifest": f.reference(".context/putnami.json")}},
		"requirements": fxArr{workflowFeature + "#later"}, "dependsOn": fxArr{"first"},
		"requiredStage": "ready", "stage": "planned", "nextResult": "Inspect the later check",
		"nextCycleTasks": fxArr{}, "decisions": fxArr{},
	})
	for _, item := range fxArrAt(workflow, "stages") {
		original, ok := item.(fxObj)
		if !ok {
			t.Fatal("fixture stage is not an object")
		}
		stage := fxObj{}
		for key, value := range original {
			stage[key] = value
		}
		stage["unit"], stage["state"] = "later", "pending"
		delete(stage, "receipt")
		delete(stage, "reason")
		workflow["stages"] = append(fxArrAt(workflow, "stages"), stage)
	}
	proof := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
	group := fxObjAt(proof, "groups", 0)
	group["requirements"] = append(fxArrAt(group, "requirements"), fxObj{
		"requirement": "later", "state": "missing", "checks": fxArr{fxObj{"check": "later-check", "state": "missing"}},
	})
	group["counts"] = fxObj{"specRequirements": 2, "executable": 2, "verified": 1,
		"unmapped": 0, "unexecutable": 0, "missing": 1, "stale": 0, "contradicted": 0}
	group["mode"], group["blocked"] = "report", false
	group["diagnostics"] = fxArr{fxObj{"severity": "warning", "code": "features.unobserved_requirement",
		"message": "The later check was not observed.", "field": ".context/specs/workflow.json"}}
	f.write(".context/sessions/synthetic-gate/spec-verification.json", proof)
	fxObjAt(workflow, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
	f.write(".context/workflow.json", workflow)
	ref := f.reference(".context/workflow.json")
	digest, ok := ref["sha256"].(string)
	if !ok {
		t.Fatal("workflow fixture has no digest")
	}
	request.SHA256 = digest
	preflight, err := f.checkHostedWorkflow(request)
	if err != nil || preflight.WouldCompleteAfterReady || preflight.OutcomeComplete ||
		len(preflight.ReadyUnits) != 1 || preflight.ReadyUnits[0] != "first" ||
		preflight.IncompleteReason != "workflow outcome has later requirements (1)" {
		t.Fatalf("later outcome preflight = %+v, %v", preflight, err)
	}
	request.Status = hostedPassingStatus(request.HeadSHA)
	request.ProposalRef = request.Status.Proposal.Ref
	actual, err := f.checkHostedWorkflow(request)
	if err != nil || actual.OutcomeComplete || len(actual.ReadyUnits) != 1 {
		t.Fatalf("hosted checks closed later outcome: %+v, %v", actual, err)
	}
}

func TestHostedWorkflowReadyReceiptReplaysOriginalEvidence(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "native-workflow-validation", "hosted-workflow-readiness-bound-to-required-checks")
	t.Parallel()
	f, workflow, request := hostedWorkflowFixture(t)
	request.Status = hostedPassingStatus(request.HeadSHA)
	request.ProposalRef = request.Status.Proposal.Ref
	result, err := f.checkHostedWorkflow(request)
	if err != nil || !result.OutcomeComplete || len(result.ReadyUnits) != 1 {
		t.Fatalf("native hosted result = %+v, %v", result, err)
	}
	output := fxObj{
		"proposalRef": request.ProposalRef, "headSHA": request.HeadSHA, "state": "open",
		"gate": "hosted checks passed on " + request.HeadSHA, "workflowSHA256": request.SHA256,
		"readyUnits": fxArr{"first"}, "labelsVerified": true, "assigneesVerified": true,
		"hostedWorkflowReceipt": fxObj{
			"workflow": fxObj{"path": request.File, "sha256": request.SHA256},
			"baseSHA":  request.BaseSHA, "headSHA": request.HeadSHA, "fingerprint": request.Fingerprint,
			"proposalRef": request.ProposalRef, "requiredChecks": fxArr{"hosted-gate"}, "status": request.Status,
		},
	}
	f.write(".context/hosted-result.json", output)
	fxObjAt(workflow, "units", 0)["stage"] = "ready"
	fxObjAt(workflow, "units", 0)["receipt"] = f.reference(".context/hosted-result.json")
	f.write(".context/workflow-v2.json", workflow)
	f.resetFingerprint(fxFingerprint{})
	resumed := f.invoke("--workflow", ".context/workflow-v2.json")
	f.expect(resumed, "", "workflow valid")
	if resumed.value["outcomeComplete"] != true {
		t.Fatalf("hosted receipt did not resume ready unit: %s", resumed.out)
	}
	output["readyUnits"] = fxArr{"other"}
	f.write(".context/hosted-result.json", output)
	fxObjAt(workflow, "units", 0)["receipt"] = f.reference(".context/hosted-result.json")
	f.write(".context/workflow-v2.json", workflow)
	f.resetFingerprint(fxFingerprint{})
	forged := f.invoke("--workflow", ".context/workflow-v2.json")
	f.expect(forged, "ready units differ from native replay", "")
	output["readyUnits"] = fxArr{"first"}
	f.write(".context/hosted-result.json", output)
	fxObjAt(workflow, "units", 0)["receipt"] = f.reference(".context/hosted-result.json")
	f.write(".context/workflow-v2.json", workflow)
	f.write(request.File, "overwritten original workflow\n")
	f.resetFingerprint(fxFingerprint{})
	stale := f.invoke("--workflow", ".context/workflow-v2.json")
	f.expect(stale, "evidence digest changed", "")
}
