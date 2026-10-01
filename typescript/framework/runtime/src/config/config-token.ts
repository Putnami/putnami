import type { ConfigDefinition, ConfigParams, InferConfig } from './config';
import { ConfigService } from './config-service';
import type { NamedToken, Registration } from '../inject/inject.type';
import { provide } from '../inject/provider';
import { named } from '../inject/token';

// ---------------------------------------------------------------------------
// Token interning + config registry
// ---------------------------------------------------------------------------

/**
 * Token cache to ensure reference equality.
 *
 * The DI container uses `Map<Token, Provider>` which relies on reference
 * equality. Without interning, two calls to `configToken(DatabaseConfig)`
 * would produce different NamedToken objects that don't match in the Map.
 */
const tokenCache = new Map<string, NamedToken>();

/**
 * Registry of ConfigDefinitions that have been tokenized.
 * Used by Application to auto-register config providers.
 */
const configRegistry = new Map<string, ConfigDefinition>();

// ---------------------------------------------------------------------------
// configToken
// ---------------------------------------------------------------------------

/**
 * Creates a typed DI token for a config definition.
 *
 * Tokens are interned: calling `configToken(X)` with the same
 * ConfigDefinition always returns the same token object (reference equality).
 *
 * Config definitions are also tracked in a registry so that the Application
 * can auto-register providers for all tokenized configs at startup.
 *
 * @example
 * ```typescript
 * import { configToken, DatabaseConfig } from './database.config';
 *
 * const DbConfigToken = configToken(DatabaseConfig);
 * // Type: NamedToken<{ host: string; port: number; ... }>
 *
 * // In a handler:
 * endpoint()
 *   .inject({ db: configToken(DatabaseConfig) })
 *   .handle(async (ctx) => {
 *     console.log(ctx.deps.db.host); // typed as string
 *   });
 *
 * // Or resolve directly:
 * const config = ctx.get(configToken(DatabaseConfig));
 * ```
 */
export function configToken<C extends ConfigDefinition>(config: C): NamedToken<InferConfig<C>> {
  const key = `config:${config.path}`;
  let token = tokenCache.get(key);
  if (!token) {
    token = named<InferConfig<C>>(key);
    tokenCache.set(key, token);
  }
  configRegistry.set(key, config);
  return token as NamedToken<InferConfig<C>>;
}

/**
 * Returns all ConfigDefinitions that have been passed to `configToken()`.
 * Used by Application to auto-register config providers at container build time.
 * @internal
 */
export function getRegisteredConfigDefinitions(): ConfigDefinition[] {
  return [...configRegistry.values()];
}

/**
 * Fail closed when a packaged capability manifest names config blocks whose
 * defining modules were not activated before the application container is
 * built. The manifest intentionally carries only a reviewable field summary,
 * not executable schema validators, so generated bundled entrypoints import
 * the manifest-declared loader modules first and use this guard to prove their
 * `configToken()` side effects populated the real runtime registry.
 *
 * @internal Generated bundled entrypoints call this during startup.
 */
export function assertRegisteredConfigDefinitions(expectedPaths: readonly string[]): void {
  const registeredPaths = new Set([...configRegistry.values()].map((definition) => definition.path));
  const missing = [...new Set(expectedPaths)].filter((path) => !registeredPaths.has(path)).sort();
  if (missing.length > 0) {
    throw new Error(
      `capability manifest config definitions were not activated: ${missing.map((path) => JSON.stringify(path)).join(', ')}`,
    );
  }
}

/**
 * Registers a dependency-contributed config definition into the registry so the
 * workload's config extraction and infra-requirements emit it transitively.
 *
 * Unlike {@link configToken}, which silently overwrites a re-registered path,
 * this rejects a path the workload — or another dependency — already registered
 * with a *different* definition, mirroring the Go merge step's duplicate-path
 * guard. Re-registering the exact same definition object is a no-op, so a
 * library whose config the workload also imports directly is fine.
 * @internal
 */
export function registerContributedConfig(config: ConfigDefinition): void {
  const key = `config:${config.path}`;
  const existing = configRegistry.get(key);
  if (existing && existing !== config) {
    throw new Error(
      `config path "${config.path}" is declared by both the workload and a dependency; ` +
        'a dependency-owned config block must use a path the workload does not define',
    );
  }
  configToken(config);
}

// ---------------------------------------------------------------------------
// provideConfig
// ---------------------------------------------------------------------------

/**
 * Creates a provider registration that resolves a config via ConfigService.
 *
 * Returns a `Registration` suitable for `ctx.register()` or `module.register()`.
 *
 * @example
 * ```typescript
 * // Register in application:
 * app.register(provideConfig(DatabaseConfig));
 *
 * // With path override for multi-datasource:
 * app.register(provideConfig(DatabaseConfig, { path: 'database.replica' }));
 *
 * // Then resolve via DI:
 * const config = ctx.get(configToken(DatabaseConfig));
 * ```
 */
export function provideConfig<C extends ConfigDefinition>(
  config: C,
  params?: ConfigParams<InferConfig<C>>,
): Registration<InferConfig<C>> {
  const token = configToken(config);
  return provide(
    token,
    (resolve) => {
      const svc = resolve(ConfigService);
      return svc.get(config, params) as InferConfig<C>;
    },
    { deps: [ConfigService] },
  );
}
