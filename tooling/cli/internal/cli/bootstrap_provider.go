package cli

import (
	"context"
	"io"
	"os"
	"slices"
	"sync"

	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// bootstrapProvider is the credential provider a user-scope extension
// declares, serving the downloads a workspace needs before its own provider
// can start: the pinned CLI the relaunch runs, and the lock-pinned extensions
// App.Run materializes before discovery. It serves the read purpose only.
// It is closed before the relaunch hands control to the pinned CLI and
// before the workspace's own provider is installed.
//
// A nil *bootstrapProvider serves nothing, and every method accepts it.
type bootstrapProvider struct {
	broker *credentialprovider.Broker
	once   sync.Once
}

// openBootstrapProvider returns the bootstrap provider of this invocation, or
// nil when it has none. It has one when four things hold: the process holds
// the run credential or the invocation enables the install provider, it runs
// in a workspace, it is not exempt from the relaunch, and an extension of the
// user scope declares the credential-provider command. Several declarers give
// a provider whose first credential fails. A workspace extension is never
// considered.
//
// It reads the user-scope lock and manifests, and starts no process: the
// provider starts on the first download that asks it for a credential. It
// denies inspection of this process before it returns a provider, as
// guardCredentials does for the workspace's provider. When that fails, it
// says so on stderr and returns nil, so this process never holds the
// provider's credential and the downloads keep the host-keyed credential. On
// a hosted run they ask no host-keyed credential and go out without one
// (extension.AuthorizeRegistryRequest).
func openBootstrapProvider(ctx context.Context, args []string, wsRoot string, providerChild bool, stderr io.Writer) *bootstrapProvider {
	if providerChild || wsRoot == "" || launch.IsExemptInvocation(args) {
		return nil
	}
	bearer, hosted := runcredential.Current()
	source, enabled := bootstrapSource(args, os.Getenv(credentialprovider.ProvidersEnv), hosted,
		os.Getenv(runnerprovider.BoundRequestEnv) != "")
	if !enabled {
		return nil
	}
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil {
		return nil
	}
	discovered, err := extension.DiscoverUserScopeExtensions(ctx, userRoot, nil)
	if err != nil || discovered == nil {
		return nil
	}
	providers := []string{runner.InvocationProviderInstall}
	broker := credentialprovider.New(userRoot, discovered.Extensions, providers, source, stderr,
		credentialprovider.WithRunCredential(bearer))
	if broker == nil {
		return nil
	}
	if err := guardCredentials(providers); err != nil {
		_ = broker.Close()
		iox.Fprintf(stderr, "putnami: the user scope's credential provider does not serve this run: %v\n", err)
		return nil
	}
	return &bootstrapProvider{broker: broker}
}

// bootstrapSource reports whether this invocation starts a bootstrap
// provider, and names why for the errors the provider's consumers see. hosted
// reports that the process holds the run credential, which always starts one.
// Otherwise the install provider does, read from the --providers flags of args
// and fromEnv, the PUTNAMI_PROVIDERS value, as invocationProviders reads them.
// bound reports a bound execution request, which decides its providers alone
// and is read after the relaunch, so without the run credential it starts
// none.
func bootstrapSource(args []string, fromEnv string, hosted, bound bool) (source string, enabled bool) {
	providers, source, err := invocationProviders(nil, ParseArgs(args, nil, nil).Global.Providers, fromEnv)
	install := err == nil && slices.Contains(credentialprovider.Purposes(providers), registry.PurposeRead)
	switch {
	case hosted && install:
		return source, true
	case hosted:
		return runcredential.Flag, true
	case bound:
		return "", false
	default:
		return source, install
	}
}

// launchBootstrap is the provider as launch.Relaunch takes it.
func (p *bootstrapProvider) launchBootstrap() launch.Bootstrap {
	if p == nil {
		return launch.Bootstrap{}
	}
	return launch.Bootstrap{Serve: p.broker.InstallRead, Close: p.close}
}

// ensureArtifactsAndClose materializes the lock-pinned artifacts of wsRoot
// with the provider serving the extension downloads, when ensure is set, and
// then shuts the provider down. The EnsureArtifacts calls that follow are
// no-ops for wsRoot, since it runs once per workspace per process.
func (p *bootstrapProvider) ensureArtifactsAndClose(ctx context.Context, wsRoot string, cfg *wsproto.Config, ensure bool) {
	if p == nil {
		return
	}
	defer p.close()
	if ensure {
		_ = lifecycle.EnsureArtifactsServing(ctx, wsRoot, cfg, p.broker.InstallRead)
	}
}

// bootstrapEnsuresArtifacts reports whether the bootstrap provider
// materializes the lock-pinned artifacts of wsRoot before it ends. It does for
// every invocation in a workspace, a structured command included: `putnami
// install`, `upgrade` and `projects sync` materialize them later, after the
// workspace's provider is installed, and a cold store must not reach that
// point with a private extension still missing. A warm store makes it a no-op.
func bootstrapEnsuresArtifacts(wsRoot string, providerMode bool) bool {
	return wsRoot != "" && !providerMode
}

// close shuts the provider down. Only the first call has an effect.
func (p *bootstrapProvider) close() {
	if p == nil {
		return
	}
	p.once.Do(func() { _ = p.broker.Close() })
}
