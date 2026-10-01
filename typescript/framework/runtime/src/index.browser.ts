/**
 * Client/Browser Exports for @putnami/runtime
 *
 * This module exports only utilities that are safe to use in browser environments.
 * Server-only features like config file loading are not available.
 */

// Error types (browser-safe)
export * from './error';
// Schema utilities (browser-safe)
export * from './schema';

import type { SchemaDefinition } from './schema';
import { validateSchema } from './schema';

/**
 * Browser stub for `tryContext`.
 *
 * Request context is backed by Node's `AsyncLocalStorage`, which is not
 * available in browsers. Returning `undefined` preserves `tryContext`'s
 * absent-context contract without pulling the server implementation into a
 * client bundle.
 */
export function tryContext<C = unknown>(): C | undefined {
  return undefined;
}

/**
 * Browser stub for `useConfig`.
 *
 * The client build has **no config sources**: there are no YAML files, no
 * `CONFIG_DATA`, no remote resolution, and no `Env()` lookups. Unlike the
 * server `useConfig`, this stub does **not** validate the result and never
 * throws on missing required fields — a browser bundle has nowhere to load
 * them from.
 *
 * It *does* apply the schema's `Default()` values so typed reads (e.g. a field
 * typed `number`) are not `undefined` in the client bundle. Resolution order
 * (lowest → highest precedence):
 *   1. schema `Default()` values
 *   2. `opts.confInit` (programmatic overrides supplied at the call site)
 *
 * Fields without a default and without a `confInit` value stay absent.
 */
export function useConfig<S extends SchemaDefinition>(
  config: { schema: S },
  opts?: { confInit?: Partial<Record<string, unknown>> },
): Record<string, unknown> {
  // Run schema validation purely to materialize Default() values. Browser
  // builds have no config sources, so validation errors (e.g. missing required
  // fields) are intentionally ignored rather than thrown.
  const { data } = validateSchema(config.schema, opts?.confInit ?? {});
  // confInit wins over defaults, mirroring the server merge order.
  return { ...data, ...(opts?.confInit ?? {}) };
}

/**
 * Browser stub for `getEnv`. The client build always reports `'browser'`;
 * there is no `NODE_ENV` / `K_SERVICE` environment detection client-side.
 */
export function getEnv(): string {
  return 'browser';
}

/**
 * Browser stub for `resetConfigLoader`. No-op on the client — there is no
 * config cache to clear because the browser build loads no config sources.
 */
export function resetConfigLoader() {
  // No-op on client
}

/**
 * Browser stub for `useRawConfigSection`. Always `undefined` on the client —
 * the browser build loads no config sources, so there is no tree to read.
 */
export function useRawConfigSection(_path: string): Record<string, unknown> | undefined {
  return undefined;
}
