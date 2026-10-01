import './env.declare';

import { getDeep } from '@putnami/utils';
import type { InferSchema, SchemaDefinition } from '../schema';
import { isSchemaDescriptor, validateSchema } from '../schema';
import { getDefaultSources, loadGlobalConfig } from './config-source';
import { runConfigLoaderResetHooks } from './loader-hooks';

// ---------------------------------------------------------------------------
// ConfigDefinition — the object produced by Config()
// ---------------------------------------------------------------------------

const CONFIG_MARKER = 'putnami:config' as const;

export interface ConfigDefinition<S extends SchemaDefinition = SchemaDefinition> {
  readonly __config: typeof CONFIG_MARKER;
  readonly path: string;
  readonly schema: S;
}

/** Extract the inferred TypeScript type from a ConfigDefinition */
export type InferConfig<C extends ConfigDefinition> = C extends ConfigDefinition<infer S> ? InferSchema<S> : never;

export function isConfigDefinition(value: unknown): value is ConfigDefinition {
  return typeof value === 'object' && value !== null && (value as ConfigDefinition).__config === CONFIG_MARKER;
}

/**
 * Define a typed configuration block.
 *
 * @param path - Dot-notation path in the YAML config file (e.g. `'database'`, `'services.api.auth'`)
 * @param schema - Schema definition describing the configuration shape
 * @returns A `ConfigDefinition` that can be passed to `useConfig()`
 *
 * @example
 * ```typescript
 * import { Config, Default, Int, Sensitive, Env } from '@putnami/runtime';
 *
 * export const DatabaseConfig = Config('database', {
 *   host: Default(String, 'localhost'),
 *   port: Default(Int, 5432),
 *   database: String,
 *   password: Sensitive(Env('DB_PASSWORD', String)),
 *   ssl: Default(Boolean, false),
 * });
 * ```
 */
export function Config<S extends SchemaDefinition>(path: string, schema: S): ConfigDefinition<S> {
  return { __config: CONFIG_MARKER, path, schema };
}

// ---------------------------------------------------------------------------
// Config loading — standalone (module-level) path
// ---------------------------------------------------------------------------

const CONF_CACHE = new Map<string, unknown>();
let globalConfig: Record<string, unknown> | undefined;

/**
 * Resets the configuration loader, clearing all cached config instances
 * and forcing a reload on the next `useConfig()` call.
 *
 * Also deactivates any active `ConfigService` delegation so that
 * `useConfig()` falls back to the standalone path. This guarantees
 * full test isolation when `process.env.CONFIG_DATA` is changed
 * between tests.
 */
export function resetConfigLoader() {
  globalConfig = undefined;
  CONF_CACHE.clear();
  _activeConfigServiceGet = undefined;
  _activeConfigServiceGetStack.length = 0;
  _activeConfigServiceRawSection = undefined;
  _activeConfigServiceRawSectionStack.length = 0;
  runConfigLoaderResetHooks();
}

/**
 * Determines the current environment name based on environment variables.
 *
 * Detection priority:
 * 1. `NODE_ENV === 'test'` → `'test'`
 * 2. `NODE_ENV === 'production'` or `NODE_ENV === 'prod'` → `'production'`
 * 3. `K_SERVICE` is set (GCP Cloud Run) → `'production'`
 * 4. Otherwise → `'local'`
 */
export function getEnv(): string {
  if (process.env.NODE_ENV === 'test') {
    return 'test';
  }
  if (process.env.NODE_ENV === 'production' || process.env.NODE_ENV === 'prod') {
    return 'production';
  }
  if (process.env.K_SERVICE) {
    return 'production';
  }
  return 'local';
}

/**
 * Parameters for loading a configuration instance.
 */
export type ConfigParams<T> = {
  /** Override the path from the ConfigDefinition */
  path?: string;
  /** Programmatic defaults to merge under the loaded configuration (YAML takes precedence) */
  confInit?: Partial<T>;
};

// ---------------------------------------------------------------------------
// Active ConfigService delegation
// ---------------------------------------------------------------------------

/**
 * Callback type for the active ConfigService's `get()` method.
 * @internal
 */
type ConfigServiceGetFn = <S extends SchemaDefinition>(
  config: ConfigDefinition<S>,
  params?: ConfigParams<InferSchema<S>>,
) => InferSchema<S>;

let _activeConfigServiceGet: ConfigServiceGetFn | undefined;
const _activeConfigServiceGetStack: ConfigServiceGetFn[] = [];

/**
 * Registers the active ConfigService's get function for delegation.
 * Called by ConfigService constructor.
 * @internal
 */
export function setActiveConfigServiceGet(fn: ConfigServiceGetFn | undefined): void {
  if (!fn) {
    _activeConfigServiceGet = undefined;
    _activeConfigServiceGetStack.length = 0;
    return;
  }

  _activeConfigServiceGetStack.push(fn);
  _activeConfigServiceGet = fn;
}

/**
 * Unregisters a ConfigService get function only if it is currently tracked.
 * If it was the active callback, restores the previous active callback.
 * @internal
 */
export function unsetActiveConfigServiceGet(fn: ConfigServiceGetFn): void {
  const idx = _activeConfigServiceGetStack.lastIndexOf(fn);
  if (idx === -1) {
    return;
  }

  _activeConfigServiceGetStack.splice(idx, 1);
  _activeConfigServiceGet = _activeConfigServiceGetStack[_activeConfigServiceGetStack.length - 1];
}

/**
 * Callback type for the active ConfigService's `rawSection()` method.
 * @internal
 */
type ConfigServiceRawSectionFn = (path: string) => Record<string, unknown> | undefined;

let _activeConfigServiceRawSection: ConfigServiceRawSectionFn | undefined;
const _activeConfigServiceRawSectionStack: ConfigServiceRawSectionFn[] = [];

/**
 * Registers the active ConfigService's rawSection function for delegation.
 * Called by ConfigService constructor.
 * @internal
 */
export function setActiveConfigServiceRawSection(fn: ConfigServiceRawSectionFn | undefined): void {
  if (!fn) {
    _activeConfigServiceRawSection = undefined;
    _activeConfigServiceRawSectionStack.length = 0;
    return;
  }

  _activeConfigServiceRawSectionStack.push(fn);
  _activeConfigServiceRawSection = fn;
}

/**
 * Unregisters a ConfigService rawSection function only if it is currently
 * tracked. If it was the active callback, restores the previous one.
 * @internal
 */
export function unsetActiveConfigServiceRawSection(fn: ConfigServiceRawSectionFn): void {
  const idx = _activeConfigServiceRawSectionStack.lastIndexOf(fn);
  if (idx === -1) {
    return;
  }

  _activeConfigServiceRawSectionStack.splice(idx, 1);
  _activeConfigServiceRawSection = _activeConfigServiceRawSectionStack[_activeConfigServiceRawSectionStack.length - 1];
}

// ---------------------------------------------------------------------------
// useConfig — primary public API
// ---------------------------------------------------------------------------

/**
 * Loads and validates a typed configuration instance.
 *
 * When a {@link ConfigService} is active (inside a DI container context),
 * delegates to ConfigService for instance-scoped caching. Otherwise, falls
 * back to the standalone module-level cache.
 *
 * Configuration is merged from the sources of {@link getDefaultSources}, highest
 * priority first:
 * 1. `CONFIG_DATA` environment variable (YAML string, priority 60)
 * 2. Registered extension sources, such as `@putnami/cloud/runtime`, at the
 *    priority each one declares
 * 3. `conf/.secrets.{env}.yaml` (gitignored local secrets, priority 35)
 * 4. `.gen/conf/.env.{env}.yaml` (merged from dependencies by ConfigPlugin, priority 30)
 * 5. `conf/.env.{env}.yaml` (environment-specific source config, priority 20)
 * 6. `conf/.env.yaml` (environment-agnostic base config, priority 10)
 *
 * Then, per field:
 * 7. Environment variables named by `Env()` descriptors — only fill keys still
 *    absent after the sources above are merged
 * 8. `confInit` values (programmatic defaults)
 * 9. `Default()` values in the schema (lowest priority)
 *
 * Results are cached by path combination.
 *
 * @param config - A `ConfigDefinition` created with `Config()`
 * @param options - Optional loading parameters
 * @returns A validated configuration object matching the schema
 *
 * @example
 * ```typescript
 * import { useConfig } from '@putnami/runtime';
 * import { DatabaseConfig } from './database.config';
 *
 * const config = useConfig(DatabaseConfig);
 * console.log(config.host); // typed as string
 *
 * // Override path for multi-datasource
 * const replica = useConfig(DatabaseConfig, { path: 'database.replica' });
 * ```
 */
export function useConfig<S extends SchemaDefinition>(
  config: ConfigDefinition<S>,
  { path: _path, confInit }: ConfigParams<InferSchema<S>> = {},
): InferSchema<S> {
  // Delegate to ConfigService if active (DI-aware path)
  if (_activeConfigServiceGet) {
    return _activeConfigServiceGet(config, { path: _path, confInit });
  }

  // Standalone path (no DI container, scripts/tests/build-time)
  return loadConfigStandalone(config, _path, confInit);
}

/**
 * Reads a section of the merged config tree without schema validation.
 *
 * This is the escape hatch for contracts whose shape a field schema cannot
 * express — for example the managed binding documents a deploy target merges
 * into a workload's resolved config (`database`, `storage`), which are keyed
 * by arbitrary logical names and validated by their own protocol parsers.
 * Typed operator-facing config should keep using `Config()` + `useConfig()`.
 *
 * When a {@link ConfigService} is active (inside a DI container context) the
 * read goes through it, so container-scoped sources are honored; otherwise the
 * standalone global tree is read. Returns `undefined` when the path is absent
 * or does not hold an object.
 */
export function useRawConfigSection(path: string): Record<string, unknown> | undefined {
  if (_activeConfigServiceRawSection) {
    return _activeConfigServiceRawSection(path);
  }

  if (!globalConfig) {
    globalConfig = loadGlobalConfig(getDefaultSources());
  }
  return rawSectionFromTree(globalConfig, path);
}

/**
 * Narrows a config-tree node at `path` to an object section.
 * Shared by the standalone path and ConfigService.
 * @internal
 */
export function rawSectionFromTree(
  tree: Record<string, unknown> | undefined,
  path: string,
): Record<string, unknown> | undefined {
  const value: unknown = getDeep(tree, path);
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  return undefined;
}

/**
 * Standalone config loading — used when no ConfigService is active.
 * @internal
 */
function loadConfigStandalone<S extends SchemaDefinition>(
  config: ConfigDefinition<S>,
  _path?: string,
  confInit?: Partial<InferSchema<S>>,
  cache?: Map<string, unknown>,
  configTree?: Record<string, unknown>,
): InferSchema<S> {
  const effectiveCache = cache || CONF_CACHE;
  const path = _path || config.path;
  const cacheKey = `${path}:${JSON.stringify(confInit || {})}`;
  const cached = effectiveCache.get(cacheKey);
  if (cached) {
    return cached as InferSchema<S>;
  }

  let tree = configTree;
  if (!tree) {
    if (!globalConfig) {
      globalConfig = loadGlobalConfig(getDefaultSources());
    }
    tree = globalConfig;
  }

  const rawConfig = (getDeep(tree, path) || {}) as Record<string, unknown>;

  const resolved = resolveEnvValues(config.schema, rawConfig);

  // Merge confInit underneath (YAML / env take precedence over programmatic defaults)
  const merged = confInit ? { ...(confInit as Record<string, unknown>), ...resolved } : resolved;

  // Validate against schema (defaults are applied during validation)
  const { data, errors } = validateSchema(config.schema, merged, {
    coerce: true,
    label: path,
  });

  if (errors.length > 0) {
    const redacted = redactErrors(errors, config.schema, path);
    throw new Error(`Config validation failed for '${path}': ${redacted}`);
  }

  effectiveCache.set(cacheKey, data);
  return data as InferSchema<S>;
}

// ---------------------------------------------------------------------------
// Internal helpers (also reused by ConfigService)
// ---------------------------------------------------------------------------

export function resolveEnvValues(schema: SchemaDefinition, raw: Record<string, unknown>): Record<string, unknown> {
  const result = { ...raw };
  for (const [key, prop] of Object.entries(schema)) {
    if (isSchemaDescriptor(prop) && prop.env && result[key] === undefined) {
      const envValue = process.env[prop.env];
      if (envValue !== undefined) {
        result[key] = envValue;
      }
    }
  }
  return result;
}

export function redactErrors(
  errors: { field: string; message: string }[],
  schema: SchemaDefinition,
  label?: string,
): string {
  return errors
    .map((e) => {
      // Resolve the descriptor by walking the schema-relative path so nested
      // sensitive fields are redacted, not just top-level ones.
      const prop = resolveSchemaProp(schema, schemaPathForError(e.field, label));
      const isSensitive = isSchemaDescriptor(prop) && prop.sensitive === true;
      return isSensitive ? `${e.field} [redacted]` : e.message;
    })
    .join(', ');
}

function schemaPathForError(field: string, label: string | undefined): string[] {
  if (label && field.startsWith(`${label}.`)) {
    return field.slice(label.length + 1).split('.');
  }
  return field.split('.');
}

/**
 * Resolves the schema descriptor for a validation error's field path.
 *
 * The field path may still include a prefix when called directly without a
 * label. Skip leading segments until one matches a top-level schema key, then
 * walk the remaining segments so nested sensitive fields are resolved, not
 * just top-level ones.
 */
function resolveSchemaProp(schema: SchemaDefinition, path: string[]): unknown {
  let start = 0;
  while (start < path.length && !Object.hasOwn(schema, path[start] as string)) {
    start++;
  }
  if (start === path.length) return undefined;

  let current: unknown = schema;
  for (let i = start; i < path.length; i++) {
    if (!current || typeof current !== 'object') return undefined;
    current = (current as Record<string, unknown>)[path[i] as string];
  }
  return current;
}
