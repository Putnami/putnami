package distributioncli

import (
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestRegistryTokenRejectsScopeBeforeCredentialResolution(t *testing.T) {
	for _, scope := range []any{"registry.package.read", "npm", "", false} {
		// No endpoint, workspace, IO or credential store is configured. Rejection
		// must happen before any of them can be read, even if a real host has a
		// cached token: this is argument validation, not an auth-dependent check.
		err := RegistryToken(map[string]any{"for": "npm", "scope": scope}, "", nil, clicore.IO{})
		if err == nil {
			t.Fatalf("scope override %v was accepted", scope)
		}
		if clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("scope override did not fail as a usage error: %v", err)
		}
	}
}
