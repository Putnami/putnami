package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A bound request without invocation.publication plans no publication, so the
// executing engine leaves its publish purpose off, says so, and leaves the
// request itself unchanged. With the block, the request's providers hold.
func TestBoundRequestWithoutPublicationEnablesNoPublishPurpose(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "gate-only-launches-no-publication", "a-request-without-the-block-enables-no-publish-purpose")
	declarers := []*extension.ExtensionDescription{credentialDeclarer("@a/x")}
	var request runner.ExecutionRequest
	request.Invocation.Providers = []string{"install", "publish"}

	var stderr strings.Builder
	providers, source := boundRequestProviders(&request.Invocation, "publish", &stderr)
	if !reflect.DeepEqual(providers, []string{"install"}) || source != providersFromRequest {
		t.Fatalf("bound request without the block = %v from %q; want install from the request", providers, source)
	}
	if !strings.Contains(stderr.String(), "publish without invocation.publication") {
		t.Fatalf("stderr = %q; want the dropped publish purpose named", stderr.String())
	}
	if !reflect.DeepEqual(request.Invocation.Providers, []string{"install", "publish"}) {
		t.Fatalf("the request's providers changed to %v", request.Invocation.Providers)
	}
	broker := credentialBroker(providers, source, "", nil, declarers, io.Discard)
	if broker == nil || !broker.Enabled(registry.PurposeRead) || broker.Enabled(registry.PurposePublish) {
		t.Fatalf("the broker serves read=%v publish=%v; want read only", broker.Enabled(registry.PurposeRead), broker.Enabled(registry.PurposePublish))
	}
	_ = broker.Close()

	only := runner.ExecutionRequest{}
	only.Invocation.Providers = []string{"publish"}
	if providers, source := boundRequestProviders(&only.Invocation, "", io.Discard); len(providers) != 0 || credentialBroker(providers, source, "", nil, declarers, io.Discard) != nil {
		t.Fatalf("a request naming only publish without the block enables %v", providers)
	}

	request.Invocation.Publication = &runner.PublicationBlock{Barrier: []string{"build"}}
	stderr.Reset()
	providers, _ = boundRequestProviders(&request.Invocation, "", &stderr)
	if !reflect.DeepEqual(providers, []string{"install", "publish"}) || stderr.Len() != 0 {
		t.Fatalf("bound request with the block = %v, stderr %q; want install and publish, silently", providers, stderr.String())
	}
}

// A process that enables publish starts its credential provider where it
// installs it, before the first hook, locally as on a hosted run. One that
// enables only install starts it on the first download. A bound request
// enables what boundRequestProviders returns, which runBoundRequest installs:
// without invocation.publication, a request that names publish drops the
// purpose, says so on stderr, and starts no provider at install. The provider
// here is a shell that records its start and exits, so the start fails after
// the record, which the command tolerates as a lazy start would. The test
// installs a read credential source, so it is not parallel.
func TestPublishPurposeStartsTheProviderBeforeTheFirstHook(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "the-publish-purpose-starts-its-provider-before-the-first-hook")
	spectest.Proves(t, "cli/provider-publication", "gate-only-launches-no-publication", "a-request-without-the-block-starts-no-publish-provider")
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	if runcredential.Hosted() {
		t.Skip("a hosted test process starts every provider eagerly")
	}
	calls := callOffsets(t, "runner_execute.go", "runBoundRequest")
	if selected, ok := calls["boundRequestProviders"]; !ok || calls["installCredentialProviders"] < selected {
		t.Fatal("runBoundRequest does not install the providers boundRequestProviders selects")
	}
	bound := func(publication *runner.PublicationBlock) *runner.ExecutionRequest {
		request := &runner.ExecutionRequest{}
		request.Invocation.Providers = []string{"install", "publish"}
		request.Invocation.Publication = publication
		return request
	}
	for _, tc := range []struct {
		name      string
		providers []string
		request   *runner.ExecutionRequest
		starts    bool
	}{
		{name: "publish", providers: []string{"publish"}, starts: true},
		{name: "install and publish", providers: []string{"install", "publish"}, starts: true},
		{name: "install", providers: []string{"install"}, starts: false},
		{name: "a bound request without the block", request: bound(nil), starts: false},
		{name: "a bound request with the block", request: bound(&runner.PublicationBlock{Barrier: []string{"build"}}), starts: true},
	} {
		marker := filepath.Join(t.TempDir(), "started")
		provider := credentialDeclarer("@a/x")
		provider.Path = t.TempDir()
		provider.Jobs[registry.CredentialProviderCommand].Command = "/bin/sh"
		provider.Jobs[registry.CredentialProviderCommand].Args = []string{"-c", "printf started > '" + marker + "'"}
		providers, source, stderr := tc.providers, providersFromFlag, &strings.Builder{}
		if tc.request != nil {
			providers, source = boundRequestProviders(&tc.request.Invocation, "", stderr)
		}

		stop, err := installCredentialProviders(providers, source, t.TempDir(), nil, []*extension.ExtensionDescription{provider}, io.Discard)
		_, statErr := os.Stat(marker)
		stop()
		if err != nil {
			t.Fatalf("%s: install = %v", tc.name, err)
		}
		if started := statErr == nil; started != tc.starts {
			t.Errorf("%s: the provider started at install = %v, want %v", tc.name, started, tc.starts)
		}
		dropped := strings.Contains(stderr.String(), "names publish without invocation.publication; the publish purpose stays off")
		if want := tc.request != nil && tc.request.Invocation.Publication == nil; dropped != want {
			t.Errorf("%s: stderr %q names the dropped publish purpose = %v, want %v", tc.name, stderr.String(), dropped, want)
		}
	}
}
