package distribution

import (
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ValidateMirrorTargets checks the bounded, non-secret intent transported by a
// release. It does not grant access to the target or decide member eligibility:
// the provider applies its target policy and stored visibility at acceptance.
func ValidateMirrorTargets(targets map[string]MirrorTarget) []diag.Diagnostic {
	if len(targets) > MaxRegistryKinds {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, "mirrors",
			"%d mirror targets exceed the limit of %d", len(targets), MaxRegistryKinds)}
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var diagnostics []diag.Diagnostic
	for _, key := range keys {
		entry := joinField("mirrors", key)
		diagnostics = append(diagnostics, validateEcosystem(entry, Ecosystem(key))...)
		target := targets[key].To
		if len(target) == 0 || len(target) > MaxMirrorTargetBytes {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, joinField(entry, "to"),
				"mirror target length is outside 1..%d bytes", MaxMirrorTargetBytes))
		} else if !mirrorTargetPattern.MatchString(target) {
			// Never echo a rejected target: a caller may have put a credential in
			// userinfo or a query string even though those forms are forbidden.
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidMirrorTarget, joinField(entry, "to"),
				"mirror target must be an HTTPS registry URL or native registry host/path without credentials, query, fragment or whitespace"))
		}
	}
	return boundDiagnostics(diagnostics)
}
