import { configRevision, type InferConfig, useConfig } from '@putnami/runtime';
import {
  databaseBindingOverride,
  ENV_DATABASE_BINDINGS,
  ENV_DATABASE_URL,
  type ResolvedDatabaseBinding,
  type ResolvedDatabaseUrl,
  resolveDatabaseBinding,
  resolveDatabaseUrl,
} from './postgres/binding';
import { PostgresConfig } from './postgres/config';
import {
  getPrimaryDatasource,
  type PrimaryDatasource,
  primaryDatasourceName,
  resolveDatasourceName,
} from './primary-datasource';

/**
 * Which datasource a `database(name, init)` call targets: the resolved logical
 * name, and the connection name its registry key carries.
 */
export function resolveConnectionTarget(
  name: string | undefined,
  init?: InferConfig<typeof PostgresConfig>,
): { resolvedName: string; connectionName?: string } {
  const resolvedName = init === undefined ? resolveDatasourceName(name) || 'default' : name || 'default';
  const connectionName = name || (init === undefined && resolvedName !== 'default' ? resolvedName : undefined);
  return { resolvedName, connectionName };
}

/**
 * effectiveConfig resolves the connection config for a logical datasource. When
 * the deployer injected a canonical database binding — the document merged into
 * the `database` config section, falling back to DATABASE_BINDINGS (see
 * resolveDatabaseBinding) — the named datasource's connection and owning schema
 * are taken from it and are authoritative: the binding's connection fields
 * override any committed `database.<name>` config, while tuning (poolSize,
 * timeouts) still comes from config/defaults. With no binding, a connection
 * URL — the `url` config field, or DATABASE_URL for the primary datasource
 * (see resolveDatabaseUrl) — supplies the connection the same way; with
 * neither, config drives the connection. The binding half is the TypeScript
 * counterpart of the Go adapter's datasource-from-binding resolution, so both
 * languages share one contract.
 */
export function effectiveConfig(name: string | undefined, init?: InferConfig<typeof PostgresConfig>): EffectiveConfig {
  if (init !== undefined) {
    return resolveEffectiveConfig(name, init);
  }
  // Every query resolves its datasource: reuse the last resolution while
  // nothing it reads has changed — the config tree, the binding pin, the two
  // environment variables and the declared primary.
  const revision = configRevision();
  const override = databaseBindingOverride();
  const bindingsEnv = process.env[ENV_DATABASE_BINDINGS];
  const urlEnv = process.env[ENV_DATABASE_URL];
  const primary = getPrimaryDatasource();
  const memo = resolved.get(name);
  if (
    memo !== undefined &&
    memo.revision === revision &&
    memo.override === override &&
    memo.bindingsEnv === bindingsEnv &&
    memo.urlEnv === urlEnv &&
    memo.primary === primary
  ) {
    return memo.result;
  }
  const result = resolveEffectiveConfig(name, undefined);
  resolved.set(name, { revision, override, bindingsEnv, urlEnv, primary, result });
  return result;
}

/** The connection config of one datasource, and where its connection came from. */
export interface EffectiveConfig {
  conf: InferConfig<typeof PostgresConfig>;
  binding?: ResolvedDatabaseBinding;
  url?: ResolvedDatabaseUrl;
  resolvedName: string;
  connectionName?: string;
}

interface ResolvedEffectiveConfig {
  revision: number;
  override: string | undefined;
  bindingsEnv: string | undefined;
  urlEnv: string | undefined;
  primary: PrimaryDatasource | undefined;
  result: EffectiveConfig;
}

/** The last resolution of each datasource name `effectiveConfig` was called with and no init. */
const resolved = new Map<string | undefined, ResolvedEffectiveConfig>();

function resolveEffectiveConfig(
  name: string | undefined,
  init: InferConfig<typeof PostgresConfig> | undefined,
): EffectiveConfig {
  const { resolvedName, connectionName } = resolveConnectionTarget(name, init);
  const path = `database.${resolvedName}`;
  const binding = resolveDatabaseBinding(resolvedName);
  const url = binding
    ? undefined
    : resolveDatabaseUrl(resolvedName, resolvedName === (primaryDatasourceName() || 'default'), init?.url);
  const connection = binding ?? url;
  // Pass the binding (or URL) connection as confInit so a required field
  // (database) is satisfied even with no config file; then re-apply it on top
  // so it wins over any committed config.
  const confInit = connection
    ? ({ ...connection.config, ...(init ?? {}) } as InferConfig<typeof PostgresConfig>)
    : init;
  const base = useConfig(PostgresConfig, { path, confInit });
  const conf = connection ? { ...base, ...connection.config } : base;
  return { conf, binding, url, resolvedName, connectionName };
}
