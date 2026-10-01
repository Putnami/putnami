package clibin

import (
	"context"
	"net/http"
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A gateway can answer one request with `502 Bad Gateway: upstream connect
// error or disconnect/reset before headers. reset reason: protocol error`.
// Archive installs retry a 5xx, and so does the CLI pin download — `putnami pin`
// and a cold launch — so that one response does not fail the command. This test
// replays that response on the CLI pin download.
//
// When fetchHTTP makes a single attempt, Pin fails with "clibin: resolver
// returned 502 Bad Gateway".
func TestCLIPinDownloadSurvivesAGatewayResetBeforeHeaders(t *testing.T) {
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	const binary = "the-pinned-cli"
	previous := extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryTokenWithCLI = previous })

	resetThenServe := func() *recorded.Server {
		return recorded.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(binary))
		}), putRegistryRecording(t, "gateway-reset-before-headers.502.http"))
	}

	pinRegistry := resetThenServe()
	t.Setenv(extension.PutRegistryURLEnv, pinRegistry.URL)
	sha, err := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli").Pin(context.Background(), "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatalf("pin after one gateway reset: %v", err)
	}

	launchRegistry := resetThenServe()
	t.Setenv(extension.PutRegistryURLEnv, launchRegistry.URL)
	path, err := NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli").Resolve(context.Background(), entryFor("1.2.3", "linux", "amd64", sha), "linux", "amd64")
	if err != nil {
		t.Fatalf("cold launch after one gateway reset: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != binary {
		t.Fatalf("cold launch admitted different bytes: err=%v", err)
	}
	if pins, launches := len(pinRegistry.Requests()), len(launchRegistry.Requests()); pins != 2 || launches != 2 {
		t.Fatalf("requests: pin %d, launch %d; want the reset and one retry each", pins, launches)
	}
}
