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
	providers, source := boundRequestProviders(&request, "publish", &stderr)
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
	if providers, source := boundRequestProviders(&only, "", io.Discard); len(providers) != 0 || credentialBroker(providers, source, "", nil, declarers, io.Discard) != nil {
		t.Fatalf("a request naming only publish without the block enables %v", providers)
	}

	request.Invocation.Publication = &runner.PublicationBlock{Barrier: []string{"build"}}
	stderr.Reset()
	providers, _ = boundRequestProviders(&request, "", &stderr)
	if !reflect.DeepEqual(providers, []string{"install", "publish"}) || stderr.Len() != 0 {
		t.Fatalf("bound request with the block = %v, stderr %q; want install and publish, silently", providers, stderr.String())
	}
}

// A process that enables publish starts its credential provider where it
// installs it, before the first hook, locally as on a hosted run. One that
// enables only install starts it on the first download. The provider here is
// a shell that records its start and exits, so the start fails after the
// record, which the command tolerates as a lazy start would. The test
// installs a read credential source, so it is not parallel.
func TestPublishPurposeStartsTheProviderBeforeTheFirstHook(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "ancestry-is-read-before-repository-code", "the-publish-purpose-starts-its-provider-before-the-first-hook")
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	if runcredential.Hosted() {
		t.Skip("a hosted test process starts every provider eagerly")
	}
	for _, tc := range []struct {
		providers []string
		starts    bool
	}{
		{[]string{"publish"}, true},
		{[]string{"install", "publish"}, true},
		{[]string{"install"}, false},
	} {
		marker := filepath.Join(t.TempDir(), "started")
		provider := credentialDeclarer("@a/x")
		provider.Path = t.TempDir()
		provider.Jobs[registry.CredentialProviderCommand].Command = "/bin/sh"
		provider.Jobs[registry.CredentialProviderCommand].Args = []string{"-c", "printf started > '" + marker + "'"}

		stop, err := installCredentialProviders(tc.providers, providersFromFlag, t.TempDir(), nil, []*extension.ExtensionDescription{provider}, io.Discard)
		_, statErr := os.Stat(marker)
		stop()
		if err != nil {
			t.Fatalf("%v: install = %v", tc.providers, err)
		}
		if started := statErr == nil; started != tc.starts {
			t.Errorf("%v: the provider started at install = %v, want %v", tc.providers, started, tc.starts)
		}
	}
}
