# ADR 0002 — A generated client carries per-operation producer lineage

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`)

## Context

During an incident the useful question is which capability, in which project,
answers a call. The generated client knows, because each method maps to one
documented operation. One client often spans several producer features, and
the language symbol is not the identity: `getV1_Operator_Cli-usage` becomes
the Go method `GetV1_Operator_Cli_usage`.

## Decision

Lineage is attached per operation. Each generated operation records the
producing project and feature, resolved from the module that owns the
`api.Plugin` the endpoint was registered on, never from the generator's module
or the client's package name.

The two fields are set together or not at all. An operation whose owning
module declares no feature is generated **unattributed**; it never inherits a
neighbouring operation's lineage.

The descriptor is keyed by the canonical operation id, the same identity the
OpenAPI `operationId` and the design graph's `generatedFrom` edges use. The
method symbol is derived separately, so a symbol rewrite cannot break the link.

## Rejected alternatives

- **One producer per client.** Wrong whenever one specification exposes
  several capabilities, which is normal.
- **Fall back to the nearest attributed operation.** Confident, wrong lineage
  that nothing downstream can detect.
- **Derive the producer from the package name or URL.** Neither identifies the
  capability.
- **Key by method symbol.** Symbols are language- and normalizer-dependent.

## Consequences

- A provider that wants attribution declares a feature on the module owning
  the api plugin.
- `FeatureTrace` consumers treat empty producer fields as a normal outcome.
