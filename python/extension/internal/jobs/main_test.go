package jobs

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/dockertest"
	"go.putnami.dev/sdk/extension/registrycred"
)

// TestMain keeps the tests from the developer's Docker configuration, and from
// starting the host's CLI to resolve a registry credential.
func TestMain(m *testing.M) {
	registrycred.ResolveToken = func(string) (string, string) { return "", "" }
	restore := dockertest.IsolateConfig()
	code := m.Run()
	restore()
	os.Exit(code)
}
