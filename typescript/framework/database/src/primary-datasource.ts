/**
 * The workload's *primary* datasource — declared once via `sql({ datasource })`
 * — and the process-global state the runtime data path reads to honour it.
 *
 * The primary datasource is the default a `Table()` resolves to when it sets no
 * explicit `db`, and the target the `/healthz` probe pings. It mirrors the Go
 * adapter's `database.PluginConfig.Datasource`: a workload names the logical
 * datasource it lives on (e.g. `identity`), and the pool, health probe, and
 * table reads/writes all follow that name — no per-`Table` `db:` repetition and
 * no hardcoded `default`.
 *
 * The state is process-global on purpose: a Putnami workload is one application
 * per process (serverless-first), so a single declared primary is unambiguous.
 * The `sql()` plugin sets it during warmup; the `Repository` and connection
 * factory read it. Tests that exercise the data path set it directly (and reset
 * it to `undefined` for isolation).
 */

/** A resolved primary datasource: its logical name and optional owning schema. */
export interface PrimaryDatasource {
  /** Logical datasource name — keys `database.<name>` config and the binding. */
  readonly name: string;
  /**
   * Owning schema applied as the primary connection's `search_path` when no
   * deploy binding supplies one (a local-dev convenience). At runtime a
   * `DATABASE_BINDINGS` entry's schema always wins; this is the fallback so a
   * workload on a non-`public` schema works without a binding.
   */
  readonly schema?: string;
}

let primary: PrimaryDatasource | undefined;

/**
 * Record (or clear, with `undefined`) the workload's primary datasource. Called
 * by the `sql()` plugin during warmup; exported for tests, which set it to drive
 * the data path and reset it to `undefined` between cases.
 */
export function setPrimaryDatasource(datasource: PrimaryDatasource | undefined): void {
  primary = datasource;
}

/** The workload's declared primary datasource, or `undefined` when none was set. */
export function getPrimaryDatasource(): PrimaryDatasource | undefined {
  return primary;
}

/**
 * The primary datasource's name, or `undefined` when none is declared. A `Table`
 * with no `db` inherits this; with no primary it stays `undefined` and resolves
 * to `default` downstream — the canonical datasource name.
 */
export function primaryDatasourceName(): string | undefined {
  return primary?.name;
}

/**
 * Resolve the datasource a consumer targets: its explicit choice when set,
 * otherwise the declared primary, otherwise `undefined` (→ `default` downstream).
 * This is the rule a `Table()` follows — an explicit `db` wins, else it inherits
 * the workload primary — and the `Repository` resolves its connection through it.
 */
export function resolveDatasourceName(explicitDatasource: string | undefined): string | undefined {
  return explicitDatasource || primary?.name;
}

/**
 * The `search_path` the primary declares for the datasource `resolvedName`
 * resolves to, or `undefined` when it is not the primary or carries no schema.
 * Only the primary datasource owns a plugin-declared schema; every other named
 * datasource takes its schema from a binding (deploy) or the server default
 * (local dev). The connection factory applies this only when no binding is
 * present, so a deploy binding's schema always wins.
 */
export function primarySearchPath(resolvedName: string): string | undefined {
  if (primary?.schema && resolvedName === primary.name) {
    return primary.schema;
  }
  return undefined;
}

/**
 * Normalise the `sql({ datasource })` config into a `PrimaryDatasource`. Accepts
 * the bare-name shorthand (`'identity'`) or the explicit `{ name, schema }` form.
 * A blank/absent name yields `undefined` — the plugin then keeps the `default`
 * behaviour — matching how the migration layer treats an empty datasource as
 * the default.
 */
export function normalizeDatasource(
  datasource: string | { name: string; schema?: string } | undefined,
): PrimaryDatasource | undefined {
  if (datasource === undefined) {
    return undefined;
  }
  if (typeof datasource === 'string') {
    const name = datasource.trim();
    return name ? { name } : undefined;
  }
  const name = datasource.name?.trim();
  if (!name) {
    return undefined;
  }
  const schema = datasource.schema?.trim();
  return schema ? { name, schema } : { name };
}
