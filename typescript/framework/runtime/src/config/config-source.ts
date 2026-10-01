import { fileExists, getCurrentProject, joinPath, mergeDeep, readFileContent } from '@putnami/utils';
import { isMissingOptionalCloudRuntime, loadCloudRuntimeModule } from './cloud-runtime-loader';
import { getEnv } from './config';
import { registerConfigLoaderResetHook } from './loader-hooks';

// Parse YAML via Bun's native implementation. Bun.YAML.parse returns `unknown`,
// so narrow to the config tree shape the sources contract expects.
const parseYaml = (input: string) => Bun.YAML.parse(input) as Record<string, unknown> | undefined;

// ---------------------------------------------------------------------------
// ConfigSource interface
// ---------------------------------------------------------------------------

/**
 * A pluggable configuration data source.
 *
 * Sources are loaded in priority order (highest priority wins).
 * Each source returns a raw config tree or undefined if unavailable.
 *
 * @example Custom source
 * ```typescript
 * const vaultSource: ConfigSource = {
 *   name: 'vault',
 *   priority: 90,
 *   load: () => fetchVaultSecrets(),
 * };
 * ```
 */
export interface ConfigSource {
  /** Human-readable name for debugging/describe */
  readonly name: string;
  /** Higher priority wins when merging. 0 = lowest */
  readonly priority: number;
  /** Load the raw configuration tree. Returns undefined if unavailable. */
  load(): Record<string, unknown> | undefined;
}

export type SourceDiscoverer = () => ConfigSource | undefined;

const registeredSourceDiscoverers: SourceDiscoverer[] = [];
let cloudRuntimeActivationAttempted = false;

export function registerSourceDiscoverer(discoverer: SourceDiscoverer): void {
  if (!registeredSourceDiscoverers.includes(discoverer)) {
    registeredSourceDiscoverers.push(discoverer);
  }
}

const MISSING_REMOTE_SOURCE_MESSAGE =
  'CONFIG_SERVER_URL is set but no registered config source serves it: the remote source lives in an extension package (for example @putnami/cloud/runtime) that the workload must depend on and activate with a static import (see typescript/framework/runtime/doc/configuration.md § Remote sources); a bun-compiled binary cannot resolve the optional dynamic require.';

function discoverRegisteredSources(): ConfigSource[] {
  activateCloudRuntimeIfAvailable();

  const sources: ConfigSource[] = [];
  for (const discoverer of registeredSourceDiscoverers) {
    const source = discoverer();
    if (source) {
      sources.push(source);
    }
  }

  // An operator who set CONFIG_SERVER_URL asked for remote configuration. If no
  // registered source serves it, the workload would boot on schema defaults and
  // the control plane would never be called — a silence that surfaces much later
  // as an unexplained missing value. Fail here, where the cause has a name. The
  // check runs on every call, so it never depends on the once-only activation
  // attempt above. Without the variable (dev, tests) staying silent is correct.
  if (sources.length === 0 && process.env.CONFIG_SERVER_URL) {
    throw new Error(MISSING_REMOTE_SOURCE_MESSAGE);
  }

  return sources;
}

type CloudRuntimeModule = {
  register?: (registrar: {
    registerSourceDiscoverer(discoverer: SourceDiscoverer): void;
    registerConfigLoaderResetHook(reset: () => void): void;
  }) => void;
};

function activateCloudRuntimeIfAvailable(): void {
  if (cloudRuntimeActivationAttempted) {
    return;
  }
  cloudRuntimeActivationAttempted = true;

  try {
    const cloudRuntime = loadCloudRuntimeModule<CloudRuntimeModule>();
    cloudRuntime.register?.({
      registerSourceDiscoverer,
      registerConfigLoaderResetHook,
    });
  } catch (error) {
    if (isMissingOptionalCloudRuntime(error)) {
      return;
    }
    throw error;
  }
}

// ---------------------------------------------------------------------------
// Built-in sources
// ---------------------------------------------------------------------------

/**
 * Reads config from the `CONFIG_DATA` environment variable (YAML string).
 *
 * Priority 60: above the other file sources, and above the field-level
 * `Env(...)` resolution that happens during schema validation — `Env(...)`
 * only fills keys still absent after this (and other file) sources have been
 * merged, so a `CONFIG_DATA` value for the same key always wins.
 */
class EnvDataSource implements ConfigSource {
  readonly name = 'CONFIG_DATA';
  readonly priority = 60;

  load(): Record<string, unknown> | undefined {
    if (!process.env.CONFIG_DATA) {
      return undefined;
    }
    return parseYaml(process.env.CONFIG_DATA);
  }
}

/**
 * Reads config from `conf/.secrets.{env}.yaml` (gitignored local secrets).
 *
 * Priority 35: just above the build-merged config file (30) so secret values
 * can override non-sensitive defaults with the same key, but operational
 * overrides (CONFIG_DATA, field-level Env(...)) still win.
 */
class SecretsFileSource implements ConfigSource {
  readonly name = 'conf/.secrets';
  readonly priority = 35;

  load(): Record<string, unknown> | undefined {
    return loadFromFile('conf', '.secrets');
  }
}

/**
 * Reads config from `.gen/conf/.env.{env}.yaml` (generated by ConfigPlugin at build time).
 */
class GenConfFileSource implements ConfigSource {
  readonly name = '.gen/conf';
  readonly priority = 30;

  load(): Record<string, unknown> | undefined {
    return loadFromFile('.gen/conf');
  }
}

/**
 * Reads config from `conf/.env.{env}.yaml` (env-specific source config files).
 */
class ConfFileSource implements ConfigSource {
  readonly name = 'conf';
  readonly priority = 20;

  load(): Record<string, unknown> | undefined {
    return loadFromFile('conf');
  }
}

/**
 * Reads config from `conf/.env.yaml` (env-agnostic base config). Loaded for
 * every environment so settings shared across envs need only one home.
 */
class BaseConfFileSource implements ConfigSource {
  readonly name = 'conf/base';
  readonly priority = 10;

  load(): Record<string, unknown> | undefined {
    try {
      const project = getCurrentProject();
      const projectPath = project?.path || '';
      const configPath = joinPath(projectPath, 'conf', '.env.yaml');
      if (!fileExists(configPath)) {
        return undefined;
      }
      return parseYaml(readFileContent(configPath, 'utf8'));
    } catch {
      return undefined;
    }
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function loadFromFile(folder: string, prefix = '.env'): Record<string, unknown> | undefined {
  try {
    const project = getCurrentProject();
    const projectPath = project?.path || '';
    const configPath = joinPath(projectPath, folder, `${prefix}.${getEnv()}.yaml`);

    if (!fileExists(configPath)) {
      return undefined;
    }

    return parseYaml(readFileContent(configPath, 'utf8'));
  } catch {
    return undefined;
  }
}

// ---------------------------------------------------------------------------
// Default sources
// ---------------------------------------------------------------------------

/**
 * Returns the default config sources. {@link loadGlobalConfig} sorts them by
 * priority, so the array order carries no meaning.
 *
 * Priority, lowest first:
 * 1. `conf/.env.yaml` (10) — env-agnostic base
 * 2. `conf/.env.{env}.yaml` (20) — env-specific
 * 3. `.gen/conf/.env.{env}.yaml` (30) — build-merged from deps
 * 4. `conf/.secrets.{env}.yaml` (35) — local secrets (gitignored)
 * 5. Registered extension sources, such as `@putnami/cloud/runtime`, at the
 *    priority each one declares
 * 6. `CONFIG_DATA` env var (60) — inline YAML for containers
 * 7. Field-level `Env(...)` (80 — not a real priority slot, applied outside this
 *    list during schema validation) — only fills keys still absent after all of
 *    the above sources are merged, so any file/CONFIG_DATA value for the same
 *    key overrides it
 *
 * @throws when `CONFIG_SERVER_URL` is set and no registered source serves it —
 * the remote source is contributed by an extension package the workload must
 * depend on and activate with a static import.
 */
export function getDefaultSources(): ConfigSource[] {
  const sources: ConfigSource[] = [
    new BaseConfFileSource(),
    new ConfFileSource(),
    new GenConfFileSource(),
    new SecretsFileSource(),
    new EnvDataSource(),
  ];

  sources.push(...discoverRegisteredSources());

  return sources;
}

/**
 * Loads and merges a global config tree from the given sources.
 *
 * Sources are applied in ascending priority order: lower-priority sources
 * are loaded first, then higher-priority sources are deep-merged on top.
 *
 * After merging, `${VAR}` references in string values are replaced with
 * the corresponding `process.env` value. If the env var is not set, the
 * reference is left as-is so schema validation can report it.
 *
 * @returns The merged config tree, or undefined if no source returned data
 */
export function loadGlobalConfig(sources: ConfigSource[]): Record<string, unknown> | undefined {
  const sorted = [...sources].sort((a, b) => a.priority - b.priority);

  let result: Record<string, unknown> | undefined;

  for (const source of sorted) {
    const raw = source.load();
    if (raw) {
      // Strip prototype-pollution keys before mergeDeep (CONFIG_DATA and
      // remote sources are untrusted).
      const data = sanitizeConfigData(raw);
      result = result ? mergeDeep(result, data) : data;
    }
  }

  return result ? interpolateEnvVars(result) : undefined;
}

const FORBIDDEN_CONFIG_KEYS = new Set(['__proto__', 'constructor', 'prototype']);

/**
 * Recursively removes prototype-pollution keys (`__proto__`, `constructor`,
 * `prototype`) from parsed config. Untrusted YAML/JSON (CONFIG_DATA, remote
 * config/secrets servers, files) can carry an own `__proto__` key whose deep
 * merge would pollute `Object.prototype` process-wide.
 */
function sanitizeConfigData<T>(value: T): T {
  if (Array.isArray(value)) {
    return value.map((item) => sanitizeConfigData(item)) as unknown as T;
  }
  if (value && typeof value === 'object') {
    const clean: Record<string, unknown> = {};
    for (const [key, val] of Object.entries(value as Record<string, unknown>)) {
      if (FORBIDDEN_CONFIG_KEYS.has(key)) continue;
      clean[key] = sanitizeConfigData(val);
    }
    return clean as unknown as T;
  }
  return value;
}

// ---------------------------------------------------------------------------
// Environment variable interpolation
// ---------------------------------------------------------------------------

const ENV_VAR_RE = /\$\{([^}:]+?)(?::-(.*?))?\}/g;

/**
 * Replaces `${VAR}` (and `${VAR:-default}`) references in a single string
 * with the corresponding `process.env` value.
 *
 * When the entire string is a single reference, the raw env value is returned
 * (so YAML type coercion is preserved). Unresolved references without a
 * default are left as-is so schema validation can report them.
 */
function interpolateString(value: string): string {
  // If the entire value is a single ${VAR} (optionally with default),
  // return the env value directly.
  const singleMatch = /^\$\{([^}:]+?)(?::-(.*?))?\}$/.exec(value);
  if (singleMatch) {
    const envVal = process.env[singleMatch[1]];
    if (envVal !== undefined) return envVal;
    if (singleMatch[2] !== undefined) return singleMatch[2];
    return value; // leave as-is so validation reports it
  }

  // Embedded references — string interpolation
  return value.replace(ENV_VAR_RE, (match, name: string, fallback: string | undefined) => {
    const envVal = process.env[name];
    if (envVal !== undefined) return envVal;
    if (fallback !== undefined) return fallback;
    return match; // leave unresolved
  });
}

/**
 * Recursively walks a config tree and replaces `${VAR}` references in string
 * values with the corresponding `process.env` value.
 */
function interpolateEnvVars<T>(value: T): T {
  if (typeof value === 'string') {
    return (value.includes('${') ? interpolateString(value) : value) as T;
  }
  if (Array.isArray(value)) {
    return value.map(interpolateEnvVars) as T;
  }
  if (value !== null && typeof value === 'object') {
    const result: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      result[k] = interpolateEnvVars(v);
    }
    return result as T;
  }
  return value;
}
