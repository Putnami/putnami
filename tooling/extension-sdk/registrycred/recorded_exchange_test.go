package registrycred

import (
	"path/filepath"
	"testing"

	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/recorded"
)

// recordedExchange loads one run of `putnami cloud registry-token` recorded in
// testdata/recorded/registry-token. The README there says where each run came
// from and what was redacted.
func recordedExchange(t *testing.T, name string) recorded.Exchange {
	t.Helper()
	return recorded.Command(t, filepath.Join("testdata", "recorded", "registry-token", name))
}

// recordedCloudCLI points the seam at a program that replays a recorded
// exchange, so a failure path answers with what the command printed rather than
// with what a test author expected it to print.
func recordedCloudCLI(t *testing.T, exchange recorded.Exchange, branches ...recorded.Branch) {
	t.Helper()
	t.Setenv(registryproto.CLIExecutableEnv, "")
	previous := cloudCLIName
	t.Cleanup(func() { cloudCLIName = previous })
	cloudCLIName = recorded.Executable(t, exchange, branches...)
}
