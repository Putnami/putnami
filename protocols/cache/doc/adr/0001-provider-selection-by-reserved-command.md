# ADR 0001: A reserved command name is the whole cache-provider selection rule

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/cache` (`protocols/cache`)

## Context

The remote cache runs out of process: core launches a cache-provider subprocess
and drives it over the provider RPC in `provider.go`. Something must decide which
loaded extension is that provider, without privileging one implementation and
without a version rule that SHA-stamped prerelease versions cannot satisfy.

## Decision

An extension serves the cache by declaring the command
`cache.ProviderCommandName` (`cache-provider`). That reserved name is the whole
rule.

- Any extension may declare it. There is no allowlist and no privileged
  implementation; the first-party cloud extension is the reference provider.
- Exactly one declarer must exist among the loaded extensions. Two declarers is
  an error.
- Core invents no version check of its own. The extension contract version is
  checked at discovery, and `InitializeResult` carries the negotiated RPC
  `ProviderProtocolVersion`.

## Rejected alternatives

- **Recognize one product name.** It encodes a vendor in a wire contract and
  makes "write your own provider" untestable.
- **An extension-version floor.** SHA-stamped prereleases do not order, so the
  floor rejects the newest builds. RPC negotiation covers real incompatibility.
- **First declarer by discovery order.** A second declarer would silently change
  which cache a workspace talks to.
- **A `cacheProvider: true` manifest field.** It duplicates the command
  declaration.

## Consequences

- `cache-provider` is public vocabulary, pinned by this module and the provider
  fixtures; renaming it breaks every provider.
- A workspace that loads two declaring extensions fails instead of caching; the
  fix is to unload one.
- An old provider can be selected, so it must degrade honestly: negotiation
  happens in `initialize`, and every op is best-effort. A provider that cannot
  serve a request makes the build slower, never wrong.
