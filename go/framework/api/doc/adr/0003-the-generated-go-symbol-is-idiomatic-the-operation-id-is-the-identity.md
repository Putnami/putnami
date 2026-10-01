# ADR 0003 — The generated Go symbol is idiomatic; the operationId stays the identity

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`)

## Context

The strict emitter names a Go method for every operation. For `POST /users`,
`go/framework/openapi` publishes the synthesized `operationId` `postUsers`,
while the reflect-route emitter and the proto/gRPC bridge derive `CreateUsers`
through `computeClientMethodName`. A foreign provider declares its own
`operationId`, such as `replaceUser`, which carries author intent. Taking the
`operationId` verbatim gives one operation two Go symbols across REST and
Connect; taking the REST idiom everywhere overrides authored names.

That idiom also drops path parameters, so `GET /v1/workspaces/{workspace}/deploy`
and `GET /v1/workspaces/{workspace}/deploy/{release}` both become
`GetV1WorkspacesDeploy`, while their canonical operationIds and TypeScript
method names stay distinct.

## Decision

**1. Authored names win; synthesized names take the idiom.** The Go symbol is
the exported form of the declared `operationId`, unless that id is one the
framework synthesizes: `CanonicalOperationID(method, path)` (what the Go
provider publishes) or `buildOperationID(method, path)` (what the reader fills
in when a document declares none). Then the symbol is
`computeClientMethodName(method, path)`.

**2. Only a collision renames, and one member keeps the symbol.** Among
operations that would share a symbol, one keeps it: an authored operationId;
otherwise the synthesized operation with the fewest path parameters, then the
smallest canonical operationId, then the smallest path and method. Every other
synthesized member takes the exported form of its operationId, which keeps
every path parameter (`GetV1WorkspacesWorkspaceDeployRelease`); that is the
authored-id form and the TypeScript name capitalized, so no third scheme
exists. The choice depends on method, path and operationId only, never on
registration order.

**3. One function decides, after omission.** `strictMethodSymbols` resolves
every symbol once, over the contract `go.omitOperations` leaves. The emitter
and the ownership manifest both read it, so `client.gen.go` and
`client.putnami.json` always name an operation alike.

**4. A collision that survives is refused.** Two authored operationIds that
normalize to one symbol, or a renamed symbol that meets an authored one, fail
with `api.clientgen_collision`.

The `operationId` stays the wire and contract identity: `client.MustOperation`
carries it, the manifest records it, the design graph joins on it, and error
decoding and telemetry report it. A declared success body is returned by
pointer (`(*User, error)`).

## Consequences

- An operation keeps one Go symbol across REST and Connect.
- A provider that authors `postUsers` for `POST /users` gets `CreateUsers`:
  the framework cannot read intent into a name it would have synthesized.
- Adding a route can rename a synthesized sibling, and removing one can give
  it back its short symbol; the regenerated diff shows it.
- The proto emitter resolves collisions its own way (`uniqueRPCName`, a
  `By<Params>` suffix), so a renamed operation's Go symbol and RPC name can
  differ. Both carry the operationId, so dispatch and telemetry are unaffected.
