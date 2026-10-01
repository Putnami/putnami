package ci

import (
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
)

// BindDeploymentToPublishedReleaseSet returns the only immutable reference a
// deploy following a managed publish may consume. The function deliberately
// accepts no channel resolver: a channel is mutable intent, while the publish
// outcome is the completed Delivery evidence.
func BindDeploymentToPublishedReleaseSet(outcome *distribution.ReleaseSetPublishOutcome) (*distribution.ReleaseSetRef, []diag.Diagnostic) {
	if outcome == nil {
		return nil, []diag.Diagnostic{diag.Errorf("ci.release_set_outcome_required", "publish.data.releaseSet",
			"deploy after managed publish requires its successful immutable release-set outcome")}
	}
	diagnostics := distribution.ValidateReleaseSetPublishOutcome(outcome)
	for index := range diagnostics {
		if diagnostics[index].Field == "" {
			diagnostics[index].Field = "publish.data.releaseSet"
		} else {
			diagnostics[index].Field = "publish.data.releaseSet." + diagnostics[index].Field
		}
	}
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	bound := outcome.Ref
	return &bound, diagnostics
}

// ValidateDeploymentReleaseSetBinding proves that the deploy input is the
// exact {id,digest} returned by publish. Passing a later resolution of the same
// channel therefore fails instead of silently moving the deployment.
func ValidateDeploymentReleaseSetBinding(outcome *distribution.ReleaseSetPublishOutcome, deployRef *distribution.ReleaseSetRef) []diag.Diagnostic {
	bound, diagnostics := BindDeploymentToPublishedReleaseSet(outcome)
	if bound == nil {
		return diagnostics
	}
	if deployRef == nil {
		return append(diagnostics, diag.Errorf("ci.release_set_deploy_ref_required", "deploy.releaseSet",
			"deploy must carry the immutable release-set ref returned by publish"))
	}
	refDiagnostics := distribution.ValidateReleaseSetRef("deploy.releaseSet", *deployRef)
	diagnostics = append(diagnostics, refDiagnostics...)
	if diag.HasErrors(refDiagnostics) {
		return diagnostics
	}
	if deployRef.ID != bound.ID || deployRef.Digest != bound.Digest {
		diagnostics = append(diagnostics, diag.Errorf("ci.release_set_binding_mismatch", "deploy.releaseSet",
			"deploy release-set ref does not exactly match the successful publish outcome"))
	}
	return diagnostics
}
