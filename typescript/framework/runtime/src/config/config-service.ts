import { getDeep } from '@putnami/utils';
import type { InferSchema, SchemaDefinition } from '../schema';
import { isSchemaDescriptor, validateSchema } from '../schema';
import type { ConfigDefinition, ConfigParams } from './config';
import {
  rawSectionFromTree,
  redactErrors,
  resolveEnvValues,
  setActiveConfigServiceGet,
  setActiveConfigServiceRawSection,
  unsetActiveConfigServiceGet,
  unsetActiveConfigServiceRawSection,
} from './config';
import { type ConfigSource, getDefaultSources, loadGlobalConfig } from './config-source';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/**
 * Per-field origin tracking for debuggability.
 */
export interface FieldOrigin {
  /** Field name */
  field: string;
  /** Resolved value */
  value: unknown;
  /** Source that provided this value (e.g. 'env:DB_HOST', 'CONFIG_DATA', '.gen/conf', 'conf', 'confInit', 'default') */
  source: string;
}

/**
 * Description of a resolved config for debugging.
 */
export interface ConfigDescription<S extends SchemaDefinition = SchemaDefinition> {
  /** The config path */
  path: string;
  /** The merged and validated value */
  value: InferSchema<S>;
  /** Per-field origin tracking */
  origins: FieldOrigin[];
}

// ---------------------------------------------------------------------------
// ConfigService
// ---------------------------------------------------------------------------

/**
 * DI-aware configuration service.
 *
 * When running inside a DI container, this service replaces the module-level
 * config cache with instance-scoped state. Sources can be customized per container.
 *
 * Registered as a singleton in the DI container by Application.
 *
 * @example
 * ```typescript
 * // Resolve via DI
 * const configService = ctx.get(ConfigService);
 * const dbConfig = configService.get(DatabaseConfig);
 *
 * // Debug: see where each field value comes from
 * const desc = configService.describe(DatabaseConfig);
 * for (const origin of desc.origins) {
 *   console.log(`${origin.field}: ${origin.value} (from ${origin.source})`);
 * }
 * ```
 */
export class ConfigService {
  private cache = new Map<string, unknown>();
  private globalConfig: Record<string, unknown> | undefined;
  private sources: ConfigSource[];
  private readonly delegateGet: <S extends SchemaDefinition>(
    config: ConfigDefinition<S>,
    params?: ConfigParams<InferSchema<S>>,
  ) => InferSchema<S>;
  private readonly delegateRawSection: (path: string) => Record<string, unknown> | undefined;

  constructor(sources?: ConfigSource[]) {
    this.sources = sources ?? getDefaultSources();
    this.delegateGet = (config, params) => this.get(config, params);
    this.delegateRawSection = (path) => this.rawSection(path);
    setActiveConfigServiceGet(this.delegateGet);
    setActiveConfigServiceRawSection(this.delegateRawSection);
  }

  /**
   * Load and validate a typed configuration instance.
   * Same semantics as the free `useConfig()` function, but with
   * instance-scoped caching (not module-level globals).
   */
  get<S extends SchemaDefinition>(
    config: ConfigDefinition<S>,
    params: ConfigParams<InferSchema<S>> = {},
  ): InferSchema<S> {
    const path = params.path || config.path;
    const cacheKey = `${path}:${JSON.stringify(params.confInit || {})}`;
    const cached = this.cache.get(cacheKey);
    if (cached) {
      return cached as InferSchema<S>;
    }

    if (!this.globalConfig) {
      this.globalConfig = loadGlobalConfig(this.sources);
    }

    const rawConfig = (getDeep(this.globalConfig, path) || {}) as Record<string, unknown>;

    const resolved = resolveEnvValues(config.schema, rawConfig);

    // Merge confInit underneath (YAML / env take precedence over programmatic defaults)
    const merged = params.confInit ? { ...(params.confInit as Record<string, unknown>), ...resolved } : resolved;

    // Validate against schema (defaults are applied during validation)
    const { data, errors } = validateSchema(config.schema, merged, {
      coerce: true,
      label: path,
    });

    if (errors.length > 0) {
      const redacted = redactErrors(errors, config.schema, path);
      throw new Error(`Config validation failed for '${path}': ${redacted}`);
    }

    this.cache.set(cacheKey, data);
    return data as InferSchema<S>;
  }

  /**
   * Describe a config with per-field origin tracking.
   *
   * Returns the merged value plus the source that provided each field,
   * making it easy to debug "why does this field have this value?"
   *
   * @example
   * ```typescript
   * const desc = configService.describe(DatabaseConfig);
   * // desc.origins = [
   * //   { field: 'host', value: 'prod.db', source: 'CONFIG_DATA' },
   * //   { field: 'port', value: 5432, source: 'default' },
   * //   { field: 'password', value: '***', source: 'env:DB_PASSWORD' },
   * // ]
   * ```
   */
  describe<S extends SchemaDefinition>(
    config: ConfigDefinition<S>,
    params: ConfigParams<InferSchema<S>> = {},
  ): ConfigDescription<S> {
    const path = params.path || config.path;

    const sorted = [...this.sources].sort((a, b) => b.priority - a.priority);
    const sourceValues: Array<{ name: string; values: Record<string, unknown> }> = [];

    for (const source of sorted) {
      const raw = source.load();
      if (raw) {
        const segment = getDeep(raw, path) as Record<string, unknown> | undefined;
        if (segment && typeof segment === 'object') {
          sourceValues.push({ name: source.name, values: segment });
        }
      }
    }

    // confInit sits below the sources and below Env(...), as in get().
    const confInit = params.confInit as Record<string, unknown> | undefined;

    const origins: FieldOrigin[] = [];
    for (const [field, schemaProp] of Object.entries(config.schema)) {
      let source = 'default';
      let value: unknown;

      // Check env var first (only if value not set in higher-priority sources)
      let envSource: string | undefined;
      let envValue: unknown;
      if (isSchemaDescriptor(schemaProp) && schemaProp.env) {
        const ev = process.env[schemaProp.env];
        if (ev !== undefined) {
          envSource = `env:${schemaProp.env}`;
          envValue = ev;
        }
      }

      // Check sources in priority order (highest first)
      let found = false;
      for (const sv of sourceValues) {
        if (sv.values[field] !== undefined) {
          source = sv.name;
          value = sv.values[field];
          found = true;
          break;
        }
      }

      // Env vars fill in when source files don't have the value
      if (!found && envSource) {
        source = envSource;
        value = envValue;
        found = true;
      }

      if (!found && confInit?.[field] !== undefined) {
        source = 'confInit';
        value = confInit[field];
        found = true;
      }

      if (!found) {
        if (isSchemaDescriptor(schemaProp) && schemaProp.default !== undefined) {
          value = schemaProp.default;
        }
      }

      const isSensitive = isSchemaDescriptor(schemaProp) && schemaProp.sensitive === true;
      origins.push({ field, value: isSensitive ? '***' : value, source });
    }

    return {
      path,
      value: this.get(config, params),
      origins,
    };
  }

  /**
   * Read a section of the merged config tree without schema validation — the
   * instance-scoped counterpart of `useRawConfigSection()`. Used for contracts
   * whose shape a field schema cannot express (e.g. the managed binding
   * documents merged into the `database`/`storage` sections), which carry
   * their own protocol validation. Returns `undefined` when the path is
   * absent or does not hold an object.
   */
  rawSection(path: string): Record<string, unknown> | undefined {
    if (!this.globalConfig) {
      this.globalConfig = loadGlobalConfig(this.sources);
    }
    return rawSectionFromTree(this.globalConfig, path);
  }

  /**
   * Invalidate cached config, forcing reload on next get().
   */
  invalidate(): void {
    this.cache.clear();
    this.globalConfig = undefined;
  }

  /**
   * Close this service and deactivate the global delegation.
   * Called by the DI container's onClose hook.
   */
  close(): void {
    unsetActiveConfigServiceGet(this.delegateGet);
    unsetActiveConfigServiceRawSection(this.delegateRawSection);
  }
}
