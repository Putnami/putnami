package ci

import (
	"errors"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
)

func TestBindDeploymentToPublishedReleaseSet(t *testing.T) {
	published := releaseSetPublishOutcome("7")
	bound, diagnostics := BindDeploymentToPublishedReleaseSet(published)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("bind: %v", diagnostics)
	}
	if bound == nil || *bound != published.Ref {
		t.Fatalf("bound = %#v, want %#v", bound, published.Ref)
	}

	// A channel may move after publish. Delivery still binds the old immutable
	// ref from the outcome; this package exposes no deploy-time resolver input.
	movedChannelHead := releaseSetRef("a")
	if movedChannelHead == *bound {
		t.Fatal("test setup did not move the channel")
	}
	if diagnostics := ValidateDeploymentReleaseSetBinding(published, bound); diag.HasErrors(diagnostics) {
		t.Fatalf("exact publish ref rejected: %v", diagnostics)
	}
	diagnostics = ValidateDeploymentReleaseSetBinding(published, &movedChannelHead)
	assertCIDiagnostic(t, diagnostics, "ci.release_set_binding_mismatch")
}

func TestDeploymentBindingFailsClosed(t *testing.T) {
	if bound, diagnostics := BindDeploymentToPublishedReleaseSet(nil); bound != nil {
		t.Fatalf("nil publish outcome produced binding %#v", bound)
	} else {
		assertCIDiagnostic(t, diagnostics, "ci.release_set_outcome_required")
	}

	published := releaseSetPublishOutcome("7")
	diagnostics := ValidateDeploymentReleaseSetBinding(published, nil)
	assertCIDiagnostic(t, diagnostics, "ci.release_set_deploy_ref_required")

	invalid := releaseSetPublishOutcome("7")
	invalid.Ref.Digest = "sha256:" + strings.Repeat("8", 64)
	if bound, findings := BindDeploymentToPublishedReleaseSet(invalid); bound != nil || !diag.HasErrors(findings) {
		t.Fatalf("invalid Distribution outcome bound: bound=%#v findings=%v", bound, findings)
	}
}

// The immutable release-set ref is runtime output, not source-controlled
// intent. Version 3 has no deploy clause at all, so the closed rule object is
// what keeps a completed, content-addressed fact from becoming author input.
func TestAuthoredCIIntentCannotCarryRuntimeReleaseSetRef(t *testing.T) {
	ref := releaseSetRef("7").ID
	authored := []byte(`{"version":3,"commands":["test"],"rules":[` +
		`{"branches":"main","publish":["canary"],"releaseSet":{"id":"` + ref + `"}}]}`)
	_, err := Parse(authored)
	if err == nil {
		t.Fatal("authored putnami.ci.json accepted a runtime release-set ref")
	}
	validation := &ValidationError{}
	ok := errors.As(err, &validation)
	if !ok {
		t.Fatalf("error = %T %v", err, err)
	}
	assertCIDiagnostic(t, validation.Diagnostics, "ci.invalid_json")
}

// releaseSetPublishOutcome is a publication that advanced two channels to the
// same immutable set: the deploy binding reads the ref, never a channel, so a
// multi-channel publication binds exactly like a single-channel one.
func releaseSetPublishOutcome(hexDigit string) *distribution.ReleaseSetPublishOutcome {
	ref := releaseSetRef(hexDigit)
	return &distribution.ReleaseSetPublishOutcome{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Ref:             ref,
		Channels: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 4},
			"next":   {Ref: ref, Generation: 2},
		},
	}
}

func releaseSetRef(hexDigit string) distribution.ReleaseSetRef {
	hex := strings.Repeat(hexDigit, 64)
	return distribution.ReleaseSetRef{ID: "rs_" + hex, Digest: "sha256:" + hex}
}

func assertCIDiagnostic(t *testing.T, diagnostics []diag.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %q in %#v", code, diagnostics)
}
