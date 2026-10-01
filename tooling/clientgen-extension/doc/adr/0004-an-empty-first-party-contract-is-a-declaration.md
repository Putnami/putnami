# ADR 0004 — An empty first-party contract is a declaration, not a missing target

- **Status**: accepted
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`), the workspace
  guard `clientgen-check` / `validate~clientgen-guard`

## Context

A provider declares its client surface with `papi.WithClientService(...)`, the
document-level `x-putnami-client` marker, and asks for clients with
`papi.Clients(...)`. An operation that names the authority owning its wire
(`x-putnami-external-contract`) is served and documented, never generated.

A provider may be entirely a standard-protocol surface, such as a Go module
proxy or an npm registry: its first-party operation set is empty while its
service identity is real and worth auditing. `go.putnami.dev/api` generates
nothing for it (`ClientsPlugin.Describe` returns before staging). The guard
must not read that absence as a broken generation, and a provider must not have
to drop `papi.WithClientService` to pass it.

## Decision

**1. The committed contract sidecar carries the declaration.** A provider
declares an empty client contract by committing `schema/openapi.json` with the
document-level `x-putnami-client` service marker and no first-party operation:
every route owned by an external authority, or no route. There is no new field
and no schema version. The sidecar is committed, so the guard reads it on a
cold clone exactly as on a built tree, as
[ADR 0003](0003-drift-is-the-generator-tasks-verdict.md) requires of every
guard input.

**2. An empty contract is satisfied with no generated target.** The guard
raises neither `clientgen.missing-manifest`, `clientgen.missing-config` nor
`clientgen.no-targets` for it, and counts nothing as required: zero required,
zero covered, 100%.

**3. Empty is not unread.** `emptyFirstPartyContract`
(`internal/workspaceclient/discovery.go`) holds only when the contract is
first-party, its document marker parsed, its first-party operation set is empty,
AND discovery read every declared operation. An unmarked operation, an invalid
`x-putnami-client` marker, a missing `operationId`, and a contradictory
`x-putnami-external-contract` (both extensions on one operation, a non-string
value, a blank authority) each count as unread and keep the provider outside
the exemption. Only a valid external marker leaves an operation out, because it
names who answers for it. One first-party operation removes the exemption.

**4. A committed target is still judged in full.** The exemption covers the
absence of a manifest, not its content. A client left behind after the last
operation moved to an external authority fails as
`clientgen.operation-coverage-drift`.
`TestACommittedTargetOfAnEmptyContractIsStillJudgedInFull` pins this, so a
widening of `inspectTarget`'s early return breaks a test.

**5. Consumers are told to classify.** `inspectTarget` reports no target for an
empty contract. `ProviderReport.EmptyContract` carries the fact, and
`bindingAdvice` ends a handwritten transport's diagnostic by telling the
developer to classify the callsite in `<consumer project>/clientgen.external.json`
under the authority that owns the wire. The diagnostic text is the only channel
that reaches the developer, because the SDK drops a failed job's data payload.
Advice to regenerate a client or declare a target would send them after work
that cannot close the finding.

## Consequences

- A 100%-external provider keeps `papi.WithClientService(...)` and marks each
  route `External`, so "deliberately out of the contract" is machine-checkable.
- Emptiness is derived from the operation set, never claimed by a flag, so the
  two cannot disagree.
- The HTTP methods the guard reads (`httpMethods` in
  `internal/workspaceclient/discovery.go`) must equal the generator's walk
  (`httpMethodOrder` in `go/framework/api/clientir.go`, `HTTP_OPERATION_KEYS`
  in `typescript/framework/client/src/generator/openapi-reader.ts`), or an
  operation under a dropped method makes a provider read as empty.
  `TestTheGuardReadsExactlyTheMethodsTheGeneratorGenerates` holds them equal.

## Alternatives rejected

- **An `emptyByDesign: true` flag** in `x-putnami-client` or the manifest
  schema. A protocol field both frameworks must emit, and a second source of
  truth that can contradict the operation set.
- **Declare it in `.gen/clientgen/config.json`.** Only a build writes it, so the
  verdict would depend on whether a build ran.
- **Suppress `clientgen.missing-manifest` whenever a manifest is absent.** Also
  passes a provider whose generation broke.
- **Drop `papi.WithClientService`.** Removes the audit record of which routes
  were left out on purpose.
