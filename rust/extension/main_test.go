package rust

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/dockertest"
)

// TestMain keeps the tests from the developer's Docker configuration.
func TestMain(m *testing.M) {
	restore := dockertest.IsolateConfig()
	code := m.Run()
	restore()
	os.Exit(code)
}
