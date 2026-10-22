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
//
// A case that names a field also pins where the code fires, for a fixture
// whose rule shares its code with other rules of the same shape.
func TestInvalidFixtureCoverage(t *testing.T) {
	lock := func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d }
	resolveRequest := func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateToolchainResolveRequest(b); return d }
	resolveResult := func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateToolchainResolveResult(b); return d }
	cases := []struct {
		fixture string
		parse   func([]byte) []diag.Diagnostic
		want    string
		field   string
	}{
		{
			"fixtures/project/invalid/missing-name.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"required-field", "",
		},
		{
			"fixtures/project/invalid/task-timeout-zero.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-timeout", "",
		},
		{
			"fixtures/project/invalid/task-timeout-negative.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-timeout", "",
		},
		{
			"fixtures/project/invalid/visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-visibility", "",
		},
		{
			"fixtures/project/invalid/distribution-visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProjectConfig(b); return d },
			"invalid-distribution-visibility", "",
		},
		{
			"fixtures/scope/invalid/distribution-visibility-unknown.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-distribution-visibility", "",
		},
		{
			"fixtures/probe/invalid/dependency-source-unknown-edge.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"unknown-dependency", "",
		},
		{
			"fixtures/probe/invalid/dependency-source-contract.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-dependency-source", "",
		},
		{
			"fixtures/scope/invalid/absolute-include.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-include", "",
		},
		{
			"fixtures/scope/invalid/line-two-placeholders.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line", "",
		},
		{
			"fixtures/scope/invalid/line-bad-chars.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line", "",
		},
		{
			"fixtures/scope/invalid/line-on-activated-scope.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateScopeConfig(b); return d },
			"invalid-line", "",
		},
		{
			"fixtures/invalid/registries-bad-key.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateWorkspaceConfig(b); return d },
			"invalid-registries", "",
		},
		{
			"fixtures/lock/invalid/bad-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-lock-version", "",
		},
		{
			"fixtures/lock/invalid/extension-missing-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field", "",
		},
		{
			"fixtures/lock/invalid/cli-missing-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field", "",
		},
		{
			"fixtures/lock/invalid/cli-source-workspace-with-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"conflicting-field", "",
		},
		{
			"fixtures/lock/invalid/toolchain-unknown-name.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"unsupported-toolchain", "",
		},
		{
			"fixtures/lock/invalid/toolchain-missing-integrities.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field", "",
		},
		{
			"fixtures/lock/invalid/toolchain-missing-source.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"required-field", "",
		},
		{
			"fixtures/lock/invalid/cli-invalid-protocol-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-protocol-version", "",
		},
		{
			"fixtures/lock/invalid/v3-with-v4-field.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"field-version", "",
		},
		{
			"fixtures/lock/invalid/agent-artifact-bad-integrity.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLock(b); return d },
			"invalid-integrity", "",
		},
		{
			"fixtures/agent-artifact/invalid/escaping-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"invalid-path", "",
		},
		{
			"fixtures/agent-artifact/invalid/duplicate-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"duplicate-path", "",
		},
		{
			"fixtures/agent-artifact/invalid/bad-integrity.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateAgentArtifactManifest(b); return d },
			"invalid-integrity", "",
		},
		// Probe v1: each reject branch that protects the digest —
		// an unsupported version, an unattributable result, a path that would
		// make the digest depend on the checkout location, a project claimed
		// twice by one provider, and metadata that cannot be bucketed.
		{
			"fixtures/probe/invalid/bad-version.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-probe-version", "",
		},
		{
			"fixtures/probe/invalid/missing-extension.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"required-field", "",
		},
		{
			"fixtures/probe/invalid/absolute-project-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-path", "",
		},
		{
			"fixtures/probe/invalid/escaping-project-path.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-path", "",
		},
		{
			"fixtures/probe/invalid/duplicate-project.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"duplicate-project", "",
		},
		{
			"fixtures/probe/invalid/non-object-metadata.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeResult(b); return d },
			"invalid-metadata", "",
		},
		{
			"fixtures/probe-request/invalid/bad-reason.json",
			func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateProbeRequest(b); return d },
			"invalid-probe-reason", "",
		},
		// Lock format 5: each toolchain entry rule, and the boundary between
		// the version 5 vocabulary and the version 3 and 4 one.
		{"fixtures/lock/invalid/v5-toolchain-with-source.json", lock, "field-version", "toolchains.go.source"},
		{"fixtures/lock/invalid/v5-toolchain-missing-archives.json", lock, "required-field", "toolchains.go.archives"},
		{"fixtures/lock/invalid/v5-toolchain-platform-mismatch.json", lock, "required-field", "toolchains.go.archives.linux/amd64"},
		{"fixtures/lock/invalid/v5-toolchain-bad-format.json", lock, "invalid-value", "toolchains.go.archives.darwin/arm64.format"},
		{"fixtures/lock/invalid/v5-toolchain-escaping-executable.json", lock, "invalid-path", "toolchains.go.archives.darwin/arm64.executable"},
		{"fixtures/lock/invalid/v5-toolchain-non-hex-integrity.json", lock, "invalid-integrity", "toolchains.go.integrities.darwin/arm64"},
		{"fixtures/lock/invalid/v5-toolchain-url-with-user-info.json", lock, "invalid-value", "toolchains.go.archives.darwin/arm64.url"},
		{"fixtures/lock/invalid/v5-toolchain-invalid-identity.json", lock, "invalid-value", "toolchains.1go"},
		{"fixtures/lock/invalid/v4-toolchain-with-archives.json", lock, "field-version", "toolchains.go.archives"},
		// Toolchain-resolve v1: a request the extension cannot fully read, and
		// a result the lock could not record as written.
		{"fixtures/toolchain-resolve-request/invalid/unknown-field.json", resolveRequest, "parse-error", ""},
		{"fixtures/toolchain-resolve-request/invalid/trailing-document.json", resolveRequest, "parse-error", ""},
		{"fixtures/toolchain-resolve-request/invalid/bad-version.json", resolveRequest, "invalid-toolchain-resolve-version", "version"},
		{"fixtures/toolchain-resolve-request/invalid/no-identities.json", resolveRequest, "required-field", "identities"},
		{"fixtures/toolchain-resolve-request/invalid/duplicate-identity.json", resolveRequest, "duplicate-value", "identities[1]"},
		{"fixtures/toolchain-resolve-request/invalid/invalid-identity.json", resolveRequest, "invalid-value", "identities[0]"},
		{"fixtures/toolchain-resolve-request/invalid/locked-not-asked.json", resolveRequest, "unknown-reference", "locked.bun"},
		{"fixtures/toolchain-resolve-request/invalid/bad-platform.json", resolveRequest, "invalid-value", "platform"},
		{"fixtures/toolchain-resolve-result/invalid/unknown-field.json", resolveResult, "parse-error", ""},
		{"fixtures/toolchain-resolve-result/invalid/bad-version.json", resolveResult, "invalid-toolchain-resolve-version", "version"},
		{"fixtures/toolchain-resolve-result/invalid/missing-extension.json", resolveResult, "required-field", "extension"},
		{"fixtures/toolchain-resolve-result/invalid/invalid-identity.json", resolveResult, "invalid-value", "toolchains.go lang"},
		{"fixtures/toolchain-resolve-result/invalid/neither-pin-nor-problem.json", resolveResult, "required-field", "toolchains.go"},
		{"fixtures/toolchain-resolve-result/invalid/pin-and-problem.json", resolveResult, "conflicting-field", "toolchains.go"},
		{"fixtures/toolchain-resolve-result/invalid/blank-problem-message.json", resolveResult, "required-field", "toolchains.go.problem.message"},
		{"fixtures/toolchain-resolve-result/invalid/pin-with-source.json", resolveResult, "field-version", "toolchains.go.pin.source"},
		{"fixtures/toolchain-resolve-result/invalid/pin-platform-mismatch.json", resolveResult, "required-field", "toolchains.go.pin.integrities.linux/amd64"},
		{"fixtures/toolchain-resolve-result/invalid/pin-bad-format.json", resolveResult, "invalid-value", "toolchains.go.pin.archives.darwin/arm64.format"},
		{"fixtures/toolchain-resolve-result/invalid/pin-escaping-executable.json", resolveResult, "invalid-path", "toolchains.go.pin.archives.darwin/arm64.executable"},
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
				if d.Code == tc.want && (tc.field == "" || d.Field == tc.field) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("fixture %s should trigger code %q on field %q, got %v", tc.fixture, tc.want, tc.field, diags)
			}
		})
	}
}
