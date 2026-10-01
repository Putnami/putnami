package sdd

import (
	"fmt"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
)

// FeatureDiffReport flattens the provisional delta into ResultV2.data and
// keeps each revision's independently derived diagnostics alongside it.
type FeatureDiffReport struct {
	featureengine.Delta
	BaseDiagnostics []diag.Diagnostic `json:"baseDiagnostics"`
	HeadDiagnostics []diag.Diagnostic `json:"headDiagnostics"`
}

// BuildFeatureDiffResult compares two immutable repository revisions without
// checking either one out or consulting the current index/worktree contents.
//
// It takes a repository root rather than a workspace view: each revision's
// membership is read from that revision's own tree, so the current selection
// would be the wrong question to ask here.
//
// Extracted from core's `FeaturesDiffCommand`, which was this body plus a
// renderer.
func BuildFeatureDiffResult(wsRoot, baseRef, headRef string) (FeatureDiffReport, error) {
	report := FeatureDiffReport{
		Delta:           *featureengine.CompareSnapshots(nil, nil),
		BaseDiagnostics: []diag.Diagnostic{},
		HeadDiagnostics: []diag.Diagnostic{},
	}

	base, baseErr := featureengine.EvaluateGitRevision(wsRoot, baseRef)
	head, headErr := featureengine.EvaluateGitRevision(wsRoot, headRef)
	if baseErr == nil {
		report.Base = base.Revision
		report.BaseDiagnostics = featureDiagnostics(base.Result.Diagnostics)
	} else {
		report.BaseDiagnostics = unresolvedFeatureRevisionDiagnostics("baseRevision")
	}
	if headErr == nil {
		report.Head = head.Revision
		report.HeadDiagnostics = featureDiagnostics(head.Result.Diagnostics)
	} else {
		report.HeadDiagnostics = unresolvedFeatureRevisionDiagnostics("headRevision")
	}

	switch {
	case baseErr != nil || headErr != nil:
		return report, WithResultData(
			protocolcli.Usagef("features diff requires two resolvable, unambiguous repository revisions"),
			report,
		)
	case base.Result.Snapshot == nil || head.Result.Snapshot == nil ||
		diag.HasErrors(base.Result.Diagnostics) || diag.HasErrors(head.Result.Diagnostics):
		errorCount := countFeatureDiagnostics(base.Result.Diagnostics).Errors + countFeatureDiagnostics(head.Result.Diagnostics).Errors
		if errorCount == 0 {
			errorCount = 1
		}
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("features diff failed with %d structural diagnostic(s)", errorCount),
			protocolcli.ErrInvalidConfig,
		), report)
	default:
		report.Delta = *featureengine.CompareSnapshots(base.Result.Snapshot, head.Result.Snapshot)
		return report, nil
	}
}

func unresolvedFeatureRevisionDiagnostics(field string) []diag.Diagnostic {
	return []diag.Diagnostic{diag.Errorf(
		featureproto.ErrorCodeInvalidSubject,
		field,
		"revision must identify one resolvable, unambiguous Git commit",
	)}
}
