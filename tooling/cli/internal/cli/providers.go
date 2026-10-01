package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// applyProviders binds one --providers occurrence: a comma list of invocation
// providers. Occurrences accumulate; the result is sorted and unique. An
// empty entry or an unknown name is a usage error.
func applyProviders(g *GlobalFlags, value string) error {
	providers, err := parseProviders(value, "--providers")
	if err != nil {
		return err
	}
	g.Providers = runner.SortStrings(append(g.Providers, providers...))
	return nil
}

// Where a process's providers came from, as its errors name it.
const (
	providersFromFlag    = "--providers"
	providersFromEnv     = credentialprovider.ProvidersEnv
	providersFromRequest = "the execution request's invocation.providers"
)

// takeProvidersEnv returns PUTNAMI_PROVIDERS and removes it from the process
// environment, so no hook, job, extension command or credential helper this
// process starts inherits the choice: it holds for this process only.
func takeProvidersEnv() string {
	value := os.Getenv(credentialprovider.ProvidersEnv)
	_ = os.Unsetenv(credentialprovider.ProvidersEnv)
	return value
}

// invocationProviders resolves the providers this process enables and names
// their source. A bound execution request decides alone: its
// invocation.providers, whatever fromEnv says. Otherwise the precedence is
// flag (--providers) > fromEnv (the PUTNAMI_PROVIDERS value) > none. The flag
// cannot name an empty list, so an empty flag means it was absent. An empty
// result is the feature off.
func invocationProviders(bound *runner.ExecutionRequest, flag []string, fromEnv string) (providers []string, source string, err error) {
	if bound != nil {
		return append([]string(nil), bound.Invocation.Providers...), providersFromRequest, nil
	}
	if len(flag) > 0 {
		return flag, providersFromFlag, nil
	}
	value := strings.TrimSpace(fromEnv)
	if value == "" {
		return nil, "", nil
	}
	providers, err = parseProviders(value, providersFromEnv)
	if err != nil {
		return nil, "", err
	}
	return runner.SortStrings(providers), providersFromEnv, nil
}

func parseProviders(value, source string) ([]string, error) {
	entries := strings.Split(value, ",")
	providers := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry)
		if !runner.KnownInvocationProvider(name) {
			return nil, usageErrorf("invalid %s value %q: expected a comma list of %s",
				source, value, strings.Join(runner.InvocationProviders, ", "))
		}
		providers = append(providers, name)
	}
	return providers, nil
}

// resolveExecutionPolicy resolves the execution flags that fall back to the
// environment and the workspace — --cpu-policy, then --providers with
// fromEnv, the PUTNAMI_PROVIDERS value — into g, and installs the credential
// provider for the providers. It returns the function that stops the provider
// and ExitSuccess, or the exit code of a usage error, with the reason written
// to stderr.
func resolveExecutionPolicy(g *GlobalFlags, cfg *wsproto.Config, commands []string, wsRoot string, extensions []*extension.ExtensionDescription, fromEnv string) (func(), int) {
	if err := resolveCPUPolicy(g, cfg, commands); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return func() {}, ExitUsage
	}
	providers, source, err := invocationProviders(nil, g.Providers, fromEnv)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return func() {}, ExitUsage
	}
	g.Providers = providers
	if err := guardCredentials(providers); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return func() {}, ExitError
	}
	stop, err := installCredentialProviders(providers, source, wsRoot, cfg, extensions, os.Stderr)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return func() {}, ExitError
	}
	return stop, ExitSuccess
}

// guardCredentials denies inspection of this process when it holds a
// credential: the run credential, or the ones providers enable. It runs
// before the credential provider is installed.
func guardCredentials(providers []string) error {
	return runcredential.Guard(len(credentialprovider.Purposes(providers)) > 0)
}

// installCredentialProviders makes the workspace's credential provider serve
// the purposes providers enable for the rest of this process, and returns the
// function that removes it and stops the provider. source names where
// providers came from. With no provider enabled, or no loaded extension
// declaring the credential-provider command, it installs nothing and every
// download keeps its host-keyed credential, except on a hosted run, where a
// download that no provider serves goes out without a credential
// (extension.AuthorizeRegistryRequest). Two declaring extensions fail the
// first download that needs the credential, not the command.
//
// extensions are the extensions this invocation already loaded; when it
// loaded none, the workspace's installed extensions are discovered as the
// dispatch path discovers them. An extension that is not installed yet
// declares nothing, so a first install fetches the provider itself on the
// host-keyed seam, or on a hosted run through the user scope's provider or
// without a credential.
//
// On a hosted run the provider starts here, before the first repository
// process, because none may start afterwards (runcredential.StartHolder). A
// provider this run refuses to start as a credential holder, because it is not
// its extension's native runtime (*runcredential.NativeHolderError) or
// repository code already ran (*runcredential.CustodyError), fails the command
// here with that refusal, before the first hook. Any other failed start answers
// the first credentials as a lazy start would.
func installCredentialProviders(providers []string, source, wsRoot string, cfg *wsproto.Config, extensions []*extension.ExtensionDescription, stderr io.Writer) (func(), error) {
	broker := credentialBroker(providers, source, wsRoot, cfg, extensions, stderr)
	if broker == nil {
		return func() {}, nil
	}
	if runcredential.Hosted() {
		if err := broker.Start(context.Background()); refusedHolder(err) {
			_ = broker.Close()
			return func() {}, err
		}
	}
	restore := broker.InstallRead()
	restoreJob := broker.InstallJobRead()
	return func() {
		restoreJob()
		restore()
		_ = broker.Close()
	}, nil
}

// refusedHolder reports whether err is a hosted run's refusal to start a
// credential holder.
func refusedHolder(err error) bool {
	var native *runcredential.NativeHolderError
	var custody *runcredential.CustodyError
	return errors.As(err, &native) || errors.As(err, &custody)
}

// credentialBroker returns the broker installCredentialProviders installs, or
// nil when it installs nothing.
func credentialBroker(providers []string, source, wsRoot string, cfg *wsproto.Config, extensions []*extension.ExtensionDescription, stderr io.Writer) *credentialprovider.Broker {
	if len(credentialprovider.Purposes(providers)) == 0 {
		return nil
	}
	if extensions == nil && wsRoot != "" {
		extensions, _ = extension.DiscoverExtensions(wsRoot, cfg, projectPathsForRoot(wsRoot))
	}
	// The hosted run's credential reaches the provider in its initialize
	// request, never in its environment.
	bearer, _ := runcredential.Current()
	return credentialprovider.New(wsRoot, extensions, providers, source, stderr, credentialprovider.WithRunCredential(bearer))
}
