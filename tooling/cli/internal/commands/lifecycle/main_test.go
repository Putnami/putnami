package lifecycle

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// Registry fixtures in this package stand up their own HTTP server and select
// it through PUTNAMI_REGISTRY_URL or the workspace's registries.put entry. A
// hosted run exports an invocation broker that wins over every authored route
// and answers 401 to a test archive, so the process drops it before any test
// runs. Tests that exercise the broker set it themselves with t.Setenv.
//
// The Go toolchain fixtures are fixtureproc programs; the process removes the
// helper build they are copies of once every test ran.
func TestMain(m *testing.M) {
	os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}
