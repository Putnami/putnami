# Runners and the Registry

A **kind** is a migration domain — `sql` today; `gcs`, `document`, `cache`,
`event-topic` are the planned domains. Each kind is served by exactly one
`MigrationRunner`, registered into the per-application `MigrationRegistry`.
This is the extensibility seam: adding a kind means implementing a runner in
your own package and registering it — no change to `@putnami/migration`.

## The Kind type

```ts
type KnownKind = 'sql' | 'gcs' | 'document' | 'cache' | 'event-topic';
type Kind = KnownKind | (string & {});
```

`KnownKind` enumerates the kinds this framework ships intent for, so editors
autocomplete them and an obvious typo like `'sqll'` stands out. `Kind` keeps a
`string & {}` escape hatch because the package is backend-free: a downstream
package may define an out-of-tree kind that the shipped union does not list.

`KindSQL` is the canonical constant for the relational-database kind.

## Implementing a MigrationRunner

```ts
import type {
  ApplyOpts,
  DriftReport,
  Kind,
  MigrationRunner,
  Record,
  RollbackOpts,
} from '@putnami/migration';

class MyRunner implements MigrationRunner {
  readonly kind: Kind = 'my-kind';

  async apply(opts?: ApplyOpts): Promise<Record[]> {
    // Run pending migrations across all targets of this kind.
    // When opts.force is false, the runner may no-op per its own
    // auto-apply policy; the migrate CLI passes force on `up`.
    return [];
  }

  async status(): Promise<Record[]> {
    // Recorded state of every migration of this kind.
    return [];
  }

  async rollback(opts?: RollbackOpts): Promise<Record[]> {
    // Empty opts → roll back the most recent applied migration.
    // opts.to → roll back everything applied AFTER the named migration.
    return [];
  }

  async verify(): Promise<DriftReport> {
    // Compare the registry view against the persisted state store.
    return { kind: this.kind };
  }
}
```

Each runner consumes every `MigrationSource` of its kind contributed by feature
plugins. The runner owns per-`(namespace, name)` duplicate detection once it has
expanded its sources into concrete definitions.

### Records and drift

`apply` / `status` / `rollback` return `Record[]` — the homogeneous, cross-kind
row shape the CLI renders the same way for every kind (`kind`, `namespace`,
`name`, `status`, `hash`, `executedAt`, `target`, …). Kind-specific extras live
in `target`.

`verify` returns a `DriftReport` describing the diff between the runner's
registry view and the state store: `hashDrifts`, `missingFromRegistry` (applied
rows absent from the current registry), and `missingFromStore` (registered
definitions not yet applied). `isDriftReportEmpty(report)` is true when registry
and store agree.

## The MigrationRegistry

One `MigrationRegistry` is held in DI by the application and built fresh at every
boot — there is no module-level singleton. You normally interact with it through
the lifecycle and the CLI, but the surface is small:

| Method | Purpose |
|--------|---------|
| `addSource(source)` | Record a feature-plugin contribution. |
| `registerRunner(runner)` | Install the one runner for a kind. |
| `sourcesFor(kind)` | Sources for a kind (defensive copy). |
| `runnerFor(kind)` | The runner for a kind, if any. |
| `kinds()` | Every kind with a runner or sources, sorted. |
| `applyAll(opts?)` | `apply` across every kind with a runner. |
| `statusAll()` | Aggregated `status` across kinds. |
| `verifyAll()` | One `DriftReport` per kind. |
| `infraManifest()` | Per-project infra manifest (see infra doc). |

`applyAll` / `statusAll` / `verifyAll` iterate `kinds()` in lexicographic order
so cross-kind output is reproducible.

## Failure modes (loud by contract)

Discoverability is a contract — failures are loud and name the offender:

- **Orphan sources.** A source contributed for a kind with no registered runner
  (e.g. a plugin ships SQL migrations but the app forgot
  `.use(databasePlugin())`) throws `UnknownKindError` for apply unless
  `allowSourceOnly` is set, plus `statusAll` and `verifyAll`, naming every
  contributing plugin. Only the automatic lifecycle apply sets
  `allowSourceOnly` so build metadata can be declared independently of the
  runtime backend.
- **Duplicate runner.** Registering a second runner for the same kind throws
  `MigrationConfigError`.
- **Invalid source.** A nil source, empty `kind`, or blank `namespace` throws
  `MigrationConfigError` at `addSource`.

Every `addSource` and `registerRunner` also emits a structured `debug` log via
`useLogger('database.migration')` — the pinned logger name of the migration
boundary (`protocols/logging/conformance`), shared with the SQL migrator and
runner — with `migration:{kind, namespace, sourcesForKind}`; routine boot
registration stays out of the default `info` output.

## Where things live

- This package: the kind-agnostic contracts + the in-process registry.
- `@putnami/database`: the SQL runner, file loaders, and drift detection.
- `@putnami/application`: wires the registry into DI (the `Application` creates
  it and publishes it into the container; the CLI reads it back via
  `getMigrationRegistry()`).
