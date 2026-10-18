package finalize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	features "go.putnami.dev/protocol/features"

	"go.putnami.dev/tooling/contributor/internal/verify"
)

// RecoverReport reads a restored verification report, as
// verify.Verifier.RecoverReport does.
type RecoverReport func(root string, report features.VerificationReportRef) ([]byte, error)

// DossierVerify checks a dossier with the verifier of `change verify`, and
// reads its verdict as the core reads its own verifier's verdict. Every
// argument is read as a flag or a value, so a dossier named -h is checked
// like any other.
func DossierVerify(root string, recoverReport RecoverReport) Verify {
	return func(_ context.Context, file, base string) (Verified, error) {
		var buffer bytes.Buffer
		verifier := verify.Verifier{Dir: root, Stdout: &buffer, RecoverReport: recoverReport}
		err := verifier.WriteVerdict([]string{"--record", file, "--base", base})
		if err != nil {
			return Verified{}, fmt.Errorf("native dossier verification: %s", strings.TrimSpace(buffer.String()))
		}
		var answer struct {
			Verdict   string `json:"verdict"`
			Stage     string `json:"stage"`
			Assurance string `json:"assurance"`
			Binding   struct {
				Fingerprint string `json:"fingerprint"`
				HeadSHA     string `json:"headSHA"`
				BaseSHA     string `json:"baseSHA"`
			} `json:"binding"`
			GateSession string `json:"gateSession"`
			Workflow    *struct {
				OutcomeComplete bool `json:"outcomeComplete"`
			} `json:"workflow"`
		}
		if err := json.Unmarshal(buffer.Bytes(), &answer); err != nil {
			return Verified{}, err
		}
		if answer.Verdict != "Putnami verified" || answer.Stage != "ready" || answer.Assurance != "local-declared-scope" {
			return Verified{}, errors.New("dossier did not produce local ready assurance")
		}
		return Verified{Fingerprint: answer.Binding.Fingerprint, HeadSHA: answer.Binding.HeadSHA,
			BaseSHA: answer.Binding.BaseSHA, GateSession: answer.GateSession,
			WorkflowPresent: answer.Workflow != nil, OutcomeComplete: answer.Workflow != nil && answer.Workflow.OutcomeComplete}, nil
	}
}

// HostedWorkflowVerify checks a workflow record against the hosted checks
// with the verifier of `change verify`.
func HostedWorkflowVerify(root string, recoverReport RecoverReport) HostedWorkflow {
	return func(ctx context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		verifier := verify.Verifier{Dir: root, RecoverReport: recoverReport}
		projection, err := verifier.CheckHostedWorkflow(ctx, verify.HostedWorkflowRequest{
			File: request.File, SHA256: request.SHA256, BaseSHA: request.BaseSHA,
			HeadSHA: request.HeadSHA, Fingerprint: request.Fingerprint, ProposalRef: request.ProposalRef,
			RequiredChecks: request.RequiredChecks, Status: request.Status,
		})
		if err != nil {
			return HostedWorkflowResult{}, err
		}
		return HostedWorkflowResult{WorkflowSHA256: projection.WorkflowSHA256,
			WouldCompleteAfterReady: projection.WouldCompleteAfterReady, OutcomeComplete: projection.OutcomeComplete,
			ReadyUnits: projection.ReadyUnits}, nil
	}
}
