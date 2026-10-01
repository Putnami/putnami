package clibin

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// brokerFixtureChildEnv marks the child test process this incident starts, so
// the child does not start one of its own.
const brokerFixtureChildEnv = "PUTNAMI_CLIBIN_BROKER_FIXTURE_CHILD"

// brokerFixtureChildTest is the fixture test the child runs. A -test.run
// pattern that matches nothing still exits 0, so the child's verbose output
// must name this test as passed.
const brokerFixtureChildTest = "TestNativePublicPinAndColdLaunchRemainAnonymousWithoutLocalCredentials"

// A native publication run exports its invocation broker,
// PUTNAMI_REGISTRY_PUT_URL, to every job, and `go test` inherits it. The broker
// wins over every authored registry route, so the package's registry fixtures
// would send their downloads to the real broker, which refuses them with 401.
// The package's TestMain unsets the variable. Pull request runs do not publish
// and never set it, so only publishing runs reach this path.
//
// The replay runs one of this package's fixture tests in a child test process
// whose environment carries a loopback broker answering with the registry's
// recorded 401 — the environment a publishing run gives `go test`.
//
// Without that TestMain, the child's pin fails with "this archive needs a
// credential".
func TestFixtureDownloadsIgnoreAHostedRunsBroker(t *testing.T) {
	if os.Getenv(brokerFixtureChildEnv) != "" {
		t.Skip("this process is the incident's child run")
	}
	broker := recorded.NewServer(t, nil, putRegistryRecording(t, "download-anonymous.401.http"))

	child := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // G204: re-runs this test binary
		"-test.run=^"+brokerFixtureChildTest+"$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(),
		extension.PrivatePutRegistryURLEnv+"="+broker.URL+"/put",
		brokerFixtureChildEnv+"=1",
	)
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("a fixture test under a publishing run's environment failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+brokerFixtureChildTest+" ") {
		t.Fatalf("the child did not run %s, so it proved nothing:\n%s", brokerFixtureChildTest, out)
	}
	if n := len(broker.Requests()); n != 0 {
		t.Fatalf("fixture downloads reached the hosted run's broker %d times", n)
	}
}
