package childenv

import (
	"slices"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// TestEnvironmentsDropTheHostChannel pins that neither environment carries
// the host channel or its token, and that both keep everything else they
// kept before.
func TestEnvironmentsDropTheHostChannel(t *testing.T) {
	t.Parallel()
	environ := []string{"PATH=/bin", wire.SocketEnv + "=/tmp/host.sock", "GIT_DIR=/elsewhere",
		wire.TokenEnv + "=launch-token", "GIT_SSH_COMMAND=ssh", "GIT_CONFIG_GLOBAL=/elsewhere/config"}
	if got, want := WithoutHostChannel(environ), []string{"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_SSH_COMMAND=ssh", "GIT_CONFIG_GLOBAL=/elsewhere/config"}; !slices.Equal(got, want) {
		t.Errorf("WithoutHostChannel = %q, want %q", got, want)
	}
	if got, want := Repository(environ), []string{"PATH=/bin", "GIT_SSH_COMMAND=ssh"}; !slices.Equal(got, want) {
		t.Errorf("Repository = %q, want %q", got, want)
	}
}
