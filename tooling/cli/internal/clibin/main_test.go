package clibin

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// Registry fixtures in this package stand up their own HTTP server and select
// it through PUTNAMI_REGISTRY_URL or the workspace's registries.put entry. A
// hosted run exports an invocation broker that wins over every authored route
// and answers 401 to a test archive, so the process drops it before any test
// runs. Tests that exercise the broker set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	os.Exit(m.Run())
}
