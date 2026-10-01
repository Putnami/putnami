# Domain access contracts (DARC)

A Domain Access & Replication Contract states what your domain promises about
facts it does not own: how fresh a copy may be, what happens when it is missing
or stale, how updates are ordered and de-duplicated, which component may write
it, how it is rebuilt, and how a deletion reaches it.

Those promises are declared in `putnami.architecture.json` and reviewed by a
human. The `darc` module of `@putnami/application` turns one of them into a
component that keeps them while the service runs.

It is the TypeScript twin of [`go.putnami.dev/app/darc`](../../../go/doc/framework/24-domain-access-contracts.md).
Both runtimes answer one shared conformance corpus — see
[Parity](#parity) at the end.

## The problem it solves

A manifest can say `maxStaleness: 5m` and the code can serve a week-old copy. No
gate catches it, because the gate reads declarations and the drift is in the
code.

The components here close that gap by taking the declaration itself as
configuration. Construction validates the contract against the architecture
protocol — the same verdict `putnami architecture validate` applies — and every
read, write, and rebuild is governed by what the contract says. The manifest
stops being a description of the code and becomes its configuration.

## Projection

A rebuildable local copy of another domain's facts.

```ts
import { Projection } from '@putnami/application';
import type { ArchitectureImport } from '@putnami/application';

const contract = {
  id: 'billing.catalog-prices.v1',
  version: 1,
  from: { domain: 'catalog', export: 'catalog.prices.v1' },
  as: 'billing.price-copy',
  mode: 'projection',
  status: 'active',
  facts: ['sku', 'unit_price'],
  bootstrap: { kind: 'api', contract: 'catalog.prices.v1', availability: 'active' },
  updates: { kind: 'event', contract: 'catalog.price-changed.v1', availability: 'active' },
  consistency: {
    maxStaleness: '5m',
    onMissing: 'fail-closed',
    onStale: 'use-stale',
    ordering: 'source-version',
    sourceVersion: 'revision',
    idempotencyKey: 'event_id',
    lateEvents: 'ignore-older',
  },
  localModel: {
    name: 'billing.price-copy',
    kind: 'projection',
    sourceIdentity: 'sku',
    projectedFields: ['sku', 'unit_price'],
    provenanceField: 'source_contract',
    observedAtField: 'observed_at',
    freshnessField: 'freshness_state',
    writer: 'billing.price-projector',
    rebuildable: true,
    rebuild: 'bootstrap',
  },
  deletion: { strategy: 'tombstone', tombstoneField: 'deleted_at' },
  justification: 'Invoicing prices a line without a synchronous call to catalog on every request.',
} as const satisfies ArchitectureImport;

const prices = new Projection<Price>(contract, {
  bootstrap: () => fetchAllPrices(),
});
await prices.rebuild();
```

Nothing here fetches. You hand the projection its bootstrap and its updates over
whatever carrier the contract declares; the component governs what happens to
them.

### Runtime precision

The architecture protocol accepts every positive Go duration, including `1ns`
and `1.5ms`, so TypeScript and Go give the same validation verdict. The
TypeScript Projection and Snapshot components measure freshness with `Date`,
which represents whole milliseconds. After protocol validation, construction
therefore requires `maxStaleness` to resolve to a positive whole number of
milliseconds; `1ms` is the smallest enforceable bound. A smaller or
fractional-millisecond declaration throws `ContractError` rather than claiming a
precision the runtime cannot keep.

### Value ownership

A projection owns the values and timestamps it stores. It clones them on write
and clones them again for `get()`, `all()`, and a stale `DarcError.record`, so
mutating an update or a returned record cannot edit the local model behind the
single writer.

The default uses the platform structured-clone algorithm, which preserves data
types such as `Date`, `Map`, `Set`, typed arrays, and cyclic graphs. Values the
algorithm cannot clone, such as functions, fail explicitly and name the option
to supply. Use `cloneValue` for those values or when an application-specific
prototype must be retained; the function must return an independent mutable
graph on every call:

```ts
const prices = new Projection<Price>(contract, {
  bootstrap: loadAll,
  cloneValue: (price) => Price.copy(price),
});
```

### Reading

`get` has three answers, and they are distinct on purpose:

| Situation | Result |
|---|---|
| Absent, `onMissing: fail-open` | `undefined` |
| Absent, otherwise | throws `DarcError` with `code: 'missing'` |
| Present, past the bound, `onStale: use-stale` or `fail-open` | the record, `freshness: 'stale'` |
| Present, past the bound, `onStale: fail-closed` | throws `DarcError` with `code: 'stale'` — and `error.record` carries what it refused, so you can log it |

`all()` applies no verdict: enumerating a projection is asking what it holds, and
the per-record freshness stamp is the answer.

### Writing

The local model names **one** writer. `writer(name)` checks the name and hands
the handle out once; a second call throws `code: 'writer-claimed'`. A second
component that wants to write has to change the declaration first, which is the
review the invariant exists to force.

```ts
const writer = prices.writer('billing.price-projector');
const changed = writer.apply({
  id: 'SKU-1',
  value: { sku: 'SKU-1', unitPrice: 1250 },
  sourceVersion: '42',
  idempotencyKey: 'evt-9',
  observedAt: new Date(),
});
```

`apply` returns `false` for the two deliveries the contract says to absorb: a
repeat of the update already applied, and — under `lateEvents: ignore-older` — an
update older than the copy held. Under `reject` that same older update throws
`code: 'late-update'` instead.

`writer.delete(id, sourceVersion)` applies the declared strategy: `tombstone`
retains a marked record, `hard-delete` removes it, `retain` leaves the copy in
place, and `not-applicable` refuses. Every accepted deletion keeps its ordering
watermark, so a delayed pre-deletion write cannot resurrect the value.

### Source version ordering

The default comparator is code point order. It is correct for RFC 3339
timestamps, ULIDs, ordered UUIDs, and zero-padded counters — and **wrong** for
unpadded decimal integers, where `"10"` sorts before `"9"`. Pass your own:

```ts
new Projection<Price>(contract, {
  bootstrap: loadAll,
  versionOrder: (left, right) => Number(left) - Number(right),
});
```

## Snapshot

An immutable, version-addressed copy. A version identifies a producer state, so
the state may be redelivered but never changed: re-attaching a version with
different content throws `code: 'immutable'`.

Snapshots use the same value-ownership contract as projections: `attach()`
clones the value and observation time, and `at()` and `latest()` return new
clones. `SnapshotOptions.cloneValue` customizes cloning for values the structured
clone algorithm cannot preserve.

```ts
const observations = new Snapshot<Batch>(contract);
observations.attach('session-7', batch, observedAt);
observations.at('session-7');   // no staleness verdict — you named the state
observations.latest();          // takes the declared consistency
```

## Command

The right to ask another domain to do something. The producer stays the
authority over whether it happens.

```ts
const ingest = new Command<UsageRecord>(contract, (payload) => post(payload), {
  onFailure: (error) => logger.warn({ error }, 'usage ingest refused'),
});

await ingest.send(record);  // surfaces the carrier's verdict
await ingest.emit(record);  // never fails the caller; the observer sees the error
```

`stats()` reports attempts and failures, because a fire-and-forget contract is
otherwise unobservable and "we sent nothing all day" must not look like
"everything failed".

## Reference

A stable handle: nothing is copied, so there is no freshness bound and no local
model. What it enforces is **minimization** — `fact()` refuses a name the import
does not carry, so reaching past the declared surface fails where it happens
rather than being found later by a reviewer comparing code with a manifest.

## Evidence

Register your components and the build records that they exist:

```ts
import { DarcPlugin } from '@putnami/application';

app.use(new DarcPlugin('billing-contracts', prices, ingest));
```

Each component contributes one row to the project's capability manifest, and
`putnami architecture validate` joins those rows to the declared imports: an
implementation nobody declared fails, and — inside a domain that already
implements something — a declaration nothing implements fails too.

The gate reads the **committed** manifest and runs no build, so a project whose
evidence must be visible has to track it. Ask for that in `putnami.json`:

```json
{ "options": { "generate": { "capabilities": true } } }
```

The build then copies what the producer emitted under `.gen/schema/` to
`schema/capabilities.json`. It is opt-in because a tracked capability manifest is
a reviewed artifact; without it the rows still exist for the build and stay
invisible to the gate.

A row is **evidence, never authority**. Emitting one cannot create a cross-domain
permission; only a reviewed manifest edit can. That is the anti-pattern
[ADR 0001](../../../protocols/architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md)
exists to forbid.

## What this module does not do

It **moves no data**. Choosing an HTTP client, an event subscription, or a file
reader stays your job, because the protocol deliberately does not rank
transports.

It **decides nothing about permissions**. Nothing here reads a workspace, and
constructing a component grants no access it was not handed.

## Parity

A second implementation of a contract's meaning is precisely the drift this
system exists to catch, so neither runtime is its own oracle. This module and its
Go twin both execute one corpus of contracts and ordered operations in
[`protocols/architecture/fixtures/conformance/`](../../../protocols/architecture/fixtures/conformance/README.md):
which contracts are refused and where, and what each of the five modes actually
does. A behavior that differs between the two languages fails on one side rather
than shipping as two runtimes that describe the same manifest differently.

Adding a case there is how a behavior becomes required. Never weaken a case to
make a runtime pass.

The one Go-only surface is the authoring builder
(`go.putnami.dev/sdk/extension/architecture`), because extensions are Go. It is
optional there too: every domain, in any language, authors its manifest with
`putnami architecture init` and `putnami architecture sync`.

## Status

The architecture protocol is **experimental**: its wire format may change without
a migration path, and this module's API is built on its types, so it moves with
it. Use it behind the same explicit opt-in.
