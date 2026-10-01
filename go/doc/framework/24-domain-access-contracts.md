# Domain access contracts (DARC)

`go.putnami.dev/app/darc` turns a Domain Access & Replication Contract into a
component that enforces it while the service runs.

The components live at `go.putnami.dev/protocol/architecture/darc`, beside the
contract vocabulary they enforce, and are framework-free: enforcing a declared
contract does not require being a Putnami application. `go.putnami.dev/app/darc`
re-exports them unchanged — the two names are the same types — and adds the one
application-owned piece, the describe plugin that emits the evidence rows shown
below. As an application author, import `go.putnami.dev/app/darc` and nothing
else changes.

> **Experimental.** `go.putnami.dev/protocol/architecture` may change its wire
> format without a migration path, and this package's API is built on its types,
> so it moves with it. Use it behind the same explicit opt-in.

## The problem it solves

A DARC import declares what a consumer domain promises about facts it does not
own: how fresh a copy may be, what happens when it is missing or stale, how
updates are ordered and de-duplicated, which component may write it, how it is
rebuilt, and how a deletion reaches it. `putnami architecture validate` checks
that the *declaration* is coherent and that the project edges implementing it are
authorized — and then stops, because the rest is behavior.

So a projection could serve a week-old copy under a declared five-minute bound
and no gate would notice. The declaration and the code were two statements about
one thing, and only one of them was checked.

The components here close that gap by **taking the declaration as
configuration**. Construction validates the contract through the protocol — the
same verdict the gate applies — and every read, write, and rebuild is governed by
what the contract says.

## Projection

A rebuildable local copy of another domain's facts.

```go
import (
    archproto "go.putnami.dev/protocol/architecture"
    "go.putnami.dev/app/darc"
)

projection, err := darc.NewProjection[WorkspaceContext](workspaceContextImport,
    darc.WithBootstrap(func(ctx context.Context) ([]darc.Update[WorkspaceContext], error) {
        return runtimeAPI.ListBindings(ctx)
    }))
if err != nil {
    return err // the contract is not one this component can enforce
}
if err := projection.Rebuild(ctx); err != nil {
    return err
}
```

Reads apply the declared freshness bound and missing/stale behavior:

```go
record, found, err := projection.Get(ctx, workspaceID)
switch {
case errors.Is(err, darc.ErrMissing):
    // onMissing is fail-closed: the fact is absent and the contract says stop.
case errors.Is(err, darc.ErrStale):
    // onStale is fail-closed: `record` is still returned, so you can log what
    // you refused.
case found && record.Freshness == darc.FreshnessStale:
    // onStale is use-stale: the copy is usable and says it is old.
}
```

Writes go through the one writer the local model names:

```go
writer, err := projection.Writer("observability.workspace-context-projector")
```

A name the local model does not declare returns `ErrNotTheWriter`; a second
handle returns `ErrWriterClaimed`. Changing who may write means changing the
declaration first, which is the review the invariant exists to force.

`Writer.Apply` compares source versions, drops a repeated delivery of the same
update, and handles an older one per `consistency.lateEvents`
(`ignore-older` → dropped silently, `reject` → `ErrLateUpdate`, `apply` →
applied). `Writer.Delete` applies the declared deletion strategy, and a
`not-applicable` contract refuses to delete at all.

### The invariants, item for item

The [projection checklist](https://putnami.dev/docs/spec-driven-development) in
the architecture protocol is the specification, and the component implements it
point by point:

| Declared | Enforced by |
|---|---|
| bootstrap and update transports | `WithBootstrap` / `WithReplay` are required exactly when the rebuild strategy names them |
| bounded staleness, missing/stale behavior | `Get` — fail-closed errors, use-stale stamps |
| ordering, idempotency, late events | `Writer.Apply` |
| provenance, observation time, freshness | `Record` carries all three as structure |
| one writer | `Projection.Writer` hands out one handle, to the declared name |
| rebuild / replay | `Projection.Rebuild` runs the declared strategy |
| deletion or tombstone | `Writer.Delete` |

### Source version ordering

The default comparator is **lexical**. That is correct for RFC 3339 timestamps,
ULIDs, ordered UUIDs, and zero-padded counters — every version shape whose byte
order is its time order — and **wrong for unpadded decimal integers**, where
`"10"` sorts before `"9"`. A contract using those must pass
`darc.WithVersionOrder`, or the late-event rule will report the opposite of what
happened.

## Snapshot

An immutable, version-addressed copy: the consumer attaches a whole producer
state under an exact version and reads it back by that version.

```go
snapshot, _ := darc.NewSnapshot[ReleaseManifest](releaseImport)
_ = snapshot.Attach(sessionID, manifest, observedAt)

record, found := snapshot.At(sessionID)          // no staleness verdict: you named the state
record, found, err := snapshot.Latest(ctx)        // the declared consistency applies here
```

Re-attaching a version with identical content is accepted — a carrier that
retries is not a producer rewriting history. Re-attaching one with different
content returns `ErrImmutable`, because every reader that already resolved that
version would otherwise be wrong.

## Command

The right to ask another domain to do something. The producer stays the
authority over whether it happens.

```go
command, err := darc.NewCommand(usageTelemetryImport,
    func(ctx context.Context, record UsageRecord) error { return ingest.Publish(ctx, record) },
    darc.WithFailureObserver[UsageRecord](func(err error) { log.Debug("usage ingest", "err", err) }))

command.Send(ctx, record)   // acknowledged: the carrier's verdict reaches you
command.Emit(ctx, record)   // fire-and-forget: it reaches the observer instead
```

`NewCommand` refuses a contract that is not **active**, including one carried by
a transport that is not active yet. A planned import is a target; sending over it
would make the same claim in code that the protocol forbids a manifest from
making.

The two shapes are the call site's choice, not the contract's — the protocol has
no fire-and-forget flag. `Emit` is what a contract like *"a refused or
unreachable ingest never fails a run"* needs. `Stats()` reports attempts and
failures, so "we sent nothing all day" and "everything failed" do not look alike.

## Reference

The thinnest of the five modes, and the most common: nothing is copied, so there
is no freshness bound, no ordering, and no local model.

```go
reference, err := darc.NewReference(protocolContractsImport)
provenance, err := reference.Fact("wire_contract_definitions")  // ErrFactNotImported otherwise
```

What that leaves to enforce is **minimization**. An import names the exact facts
it consumes because a fact nobody uses is a permission nobody needed, and
`Fact` refuses a name the contract does not carry — so reaching past the declared
surface fails where it happens rather than being found later by a reviewer
comparing code with a manifest.

## Evidence

A registered component contributes **one machine row** to the project's
capability manifest — the describe-owned sidecar, whose sole committer describe
remains:

```go
a.Use(darc.NewPlugin("observability-contracts", reference, projection, usageCommand))
```

The row carries the import ID, the mode, the status, the declared carriers with
their roles, and the enforced parameters, all verbatim from the contract the
component was constructed with. `putnami architecture validate` reads those rows
beside the declared imports and reports a declared active contract nothing
implements, or an implemented one nobody declared.

A row is **evidence, never authority**. Emitting one cannot create a cross-domain
permission; only a reviewed edit to a `putnami.architecture.json` can. The plugin
takes no part in the runtime lifecycle: it configures nothing, starts nothing,
and provides nothing into DI.

A project that registers no component publishes no rows, and the collection stays
absent from the file — so "declared but not implemented" stays a real finding
instead of being masked by an empty list.

## Wiring

A component is an ordinary value, so the usual DI conventions apply with nothing
added:

```go
app.ProvideFunc(func() (*darc.Projection[WorkspaceContext], error) {
    return darc.NewProjection[WorkspaceContext](workspaceContextImport,
        darc.WithBootstrap(loadFromRuntimeAPI))
})
```

or token-based: `module.Provide(inject.Value(projection))`.

## What this package does not do

It **moves no data**. A projection is given its bootstrap and its updates by the
consumer, over whatever carrier the contract declares; this package governs what
happens to them. Choosing an HTTP client, an event subscription, or a file reader
stays your job, because the protocol deliberately does not rank transports.

It **decides nothing about permissions**. Which domain may import which export is
declared and reviewed in a `putnami.architecture.json`; nothing here reads a
workspace, and constructing a component grants no access it was not handed.

## Parity with TypeScript

`@putnami/application` enforces the same contracts for a TypeScript workload —
see [Domain access contracts](../../../typescript/doc/framework/27-domain-access-contracts.md).
The declaration, the commands, the manifest, and the gate were always shared;
what is shared now is the runtime.

A second implementation of a contract's meaning is precisely the drift this
system exists to catch, so neither runtime is its own oracle. Both execute one
corpus of contracts and ordered operations in
[`protocols/architecture/fixtures/conformance/`](../../../protocols/architecture/fixtures/conformance/README.md):
which contracts are refused and where, and what a projection, snapshot, command,
and reference actually do. A behavior that differs between the two languages
fails on one side rather than shipping as two runtimes that describe the same
manifest differently.

Adding a case there is how a behavior becomes required. Never weaken a case to
make a runtime pass: the corpus is the contract's meaning, and a runtime that
cannot meet it is the thing that is wrong.

The one surface that stays Go-only is the authoring builder
(`go.putnami.dev/sdk/extension/architecture`), because extensions are Go. It is
optional here too — every domain, in any language, authors its manifest with
`architecture init` and `architecture sync`.
