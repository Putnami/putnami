package agentctx

import (
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// A failure names the owner and never prints a credential: userinfo and a
// query token are withheld from every rendering, and the underlying error is
// still reachable for classification.
func TestAgentContentFailuresRedactCredentials(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "no-secret-leak", "failure-messages-redact-credentials-and-locations")
	cause := errors.Join(agentartifacts.ErrCollision,
		errors.New(`fetch https://deploy:s3cr3t@registry.example/acme/contributor/download?token=abc&channel=1 failed`))

	for name, err := range map[string]error{
		"failure":       agentArtifactFailure("@acme/contributor", nil, cause),
		"state failure": agentArtifactStateFailure("@acme/contributor", cause),
	} {
		message := err.Error()
		for _, secret := range []string{"deploy", "s3cr3t", "token=abc"} {
			if strings.Contains(message, secret) {
				t.Errorf("%s printed %q: %s", name, secret, message)
			}
		}
		if !strings.Contains(message, "@acme/contributor") || !strings.Contains(message, shared.RedactedMarker) {
			t.Errorf("%s = %q, want the owner named and the withheld part marked", name, message)
		}
		if !errors.Is(err, agentartifacts.ErrCollision) {
			t.Errorf("%s lost its class: %v", name, err)
		}
	}
}
