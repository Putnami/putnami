package workspace

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestInvalidFixtureCoverage pins that the newly-corpus'd shapes' acute reject
// branches are each triggered by a fixture. shapes_conformance_test
// already asserts every invalid fixture is rejected; this asserts the specific
// validation codes fire, so relaxing a rule turns a red fixture green here.
func TestInvalidFixtureCoverage(t *testing.T) {
	cases := []struct {
		fixture string
		parse   func([]byte) []diag.Diagnostic
		want    string
	}{
		{
			"fixtures/project/invalid/missing-name.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"required-field",
		},
		{
			"fixtures/project/invalid/task-timeout-zero.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-timeout",
		},
		{
			"fixtures/project/invalid/task-timeout-negative.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-timeout",
		},
		{
			"fixtures/project/invalid/visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-visibility",
		},
		{
			"fixtures/project/invalid/distribution-visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-distribution-visibility",
		},
		{
			"fixtures/scope/invalid/distribution-visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-distribution-visibility",
		},
		{
			"fixtures/probe/invalid/dependency-source-unknown-edge.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"unknown-dependency",
		},
		{
			"fixtures/probe/invalid/dependency-source-contract.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-dependency-source",
		},
		{
			"fixtures/scope/invalid/absolute-include.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-include",
		},
		{
			"fixtures/scope/invalid/line-two-placeholders.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line",
		},
		{
			"fixtures/scope/invalid/line-bad-chars.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line",
		},
		{
			"fixtures/scope/invalid/line-on-activated-scope.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line",
		},
		{
			"fixtures/invalid/registries-bad-key.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateWorkspaceConfig(b); return d },
			"invalid-registries",
		},
		{
			"fixtures/lock/invalid/bad-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-lock-version",
		},
		{
			"fixtures/lock/invalid/extension-missing-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field",
		},
		{
			"fixtures/lock/invalid/cli-missing-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field",
		},
		{
			"fixtures/lock/invalid/cli-source-workspace-with-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"conflicting-field",
		},
		{
			"fixtures/lock/invalid/toolchain-unknown-name.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"unsupported-toolchain",
		},
		{
			"fixtures/lock/invalid/toolchain-missing-integrities.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field",
		},
		{
			"fixtures/lock/invalid/toolchain-missing-source.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field",
		},
		{
			"fixtures/lock/invalid/cli-invalid-protocol-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-protocol-version",
		},
		{
			"fixtures/lock/invalid/v3-with-v4-field.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"field-version",
		},
		{
			"fixtures/lock/invalid/agent-artifact-bad-integrity.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-integrity",
		},
		{
			"fixtures/agent-artifact/invalid/escaping-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"invalid-path",
		},
		{
			"fixtures/agent-artifact/invalid/duplicate-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"duplicate-path",
		},
		{
			"fixtures/agent-artifact/invalid/bad-integrity.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"invalid-integrity",
		},
		// Probe v1: each reject branch that protects the digest —
		// an unsupported version, an unattributable result, a path that would
		// make the digest depend on the checkout location, a project claimed
		// twice by one provider, and metadata that cannot be bucketed.
		{
			"fixtures/probe/invalid/bad-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-probe-version",
		},
		{
			"fixtures/probe/invalid/missing-extension.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"required-field",
		},
		{
			"fixtures/probe/invalid/absolute-project-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-path",
		},
		{
			"fixtures/probe/invalid/escaping-project-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-path",
		},
		{
			"fixtures/probe/invalid/duplicate-project.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"duplicate-project",
		},
		{
			"fixtures/probe/invalid/non-object-metadata.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-metadata",
		},
		{
			"fixtures/probe-request/invalid/bad-reason.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeRequest(b); return d },
			"invalid-probe-reason",
		},
	}

	for _, tc := range cases {
		t.Run(filepath.Base(tc.fixture), func(t *testing.T) {
			data, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			diags := tc.parse(data)
			found := false
			for _, d := range diags {
				if d.Code == tc.want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("fixture %s should trigger code %q, got %v", tc.fixture, tc.want, diags)
			}
		})
	}
}
