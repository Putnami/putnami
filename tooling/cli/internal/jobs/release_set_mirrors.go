package jobs

import (
	"fmt"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
)

// BuildMirrorTargets copies the declared registry destinations into the one
// release request. The CLI does not select public members or resolve backend
// credentials; it retains this policy snapshot from planning to finalization.
func BuildMirrorTargets(policy *ciproto.Distribution) (map[string]distribution.MirrorTarget, error) {
	if policy == nil {
		return nil, nil
	}
	var targets map[string]distribution.MirrorTarget
	for ecosystem, registry := range policy.Registries {
		if registry.Mirror == nil {
			continue
		}
		if targets == nil {
			targets = make(map[string]distribution.MirrorTarget)
		}
		targets[ecosystem] = distribution.MirrorTarget{To: strings.TrimSpace(registry.Mirror.To)}
	}
	if diagnostics := distribution.ValidateMirrorTargets(targets); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("distribution mirror policy: %s", firstReleaseSetDiagnostic(diagnostics))
	}
	return targets, nil
}
