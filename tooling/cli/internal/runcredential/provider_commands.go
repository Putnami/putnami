package runcredential

import (
	"slices"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
)

// providerCommands are the reserved commands through which an extension
// serves a provider capability: the CLI starts the provider for the whole run
// and hands it the run credential, a credential derived from it, or the run's
// evidence. registry.CredentialProviderCommand also serves the publish
// purpose and publication-v1, and distribution.ProviderCommandName serves the
// release sets a publish or a deploy resolves.
var providerCommands = []string{
	registry.CredentialProviderCommand,
	cache.ProviderCommandName,
	runner.ProviderCommandName,
	protocolcli.SessionReporterCommand,
	protocolcli.LogReporterCommand,
	distribution.ProviderCommandName,
}

// ProviderCommands returns the reserved commands of the providers that hold
// the run credential. A workspace whose extensions declare none of them gives
// --credential-fd no holder.
func ProviderCommands() []string {
	return slices.Clone(providerCommands)
}
