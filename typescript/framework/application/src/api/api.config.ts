import { Config, Default, Optional } from '@putnami/runtime';
import type { ClientServiceContract } from './client-contract';

/**
 * API plugin configuration.
 *
 * **Prefix resolution** (first non-undefined wins):
 * 1. Explicit `prefix` value — `api({ prefix: '/v1' })`.
 * 2. Parent module's `.path()` — `module('tasks').path('/tasks').use(api())`.
 * 3. No prefix — routes mount at the root (default).
 *
 * The scan folder name (`api` by default) is about file organisation only;
 * it does **not** affect the URL prefix. `src/api/users/get.ts` → `GET /users`.
 */
export const ApiConfig = Config('api', {
  scanFolder: Default(String, 'api'),
  autoScan: Default(Boolean, true),
  prefix: Optional(String),
  /**
   * Participate in token-based (double-submit) CSRF protection.
   *
   * Defaults to `false`: `api()` routes are exempt from the server's `csrf`
   * token check (the same-origin `originGuard` still applies). This matches the
   * common case where API routes are called by non-browser clients or guarded
   * by bearer tokens rather than cookies.
   *
   * Set to `true` to opt this plugin's routes into the server `csrf` middleware,
   * so state-changing requests must carry a valid `X-CSRF-Token` (or `_csrf`
   * field). Only meaningful when the server is configured with `csrf: true`.
   */
  csrf: Default(Boolean, false),
  /**
   * Opt into build-time AOT validation codegen.
   *
   * Defaults to `false`. When `true`, the route loader generator introspects each
   * endpoint's declared schemas at build time and emits specialised, inlined
   * validators for the scalar fields it can replicate exactly (`String`,
   * `Number`, `Boolean`, `Int`), falling back to the generic validator for
   * anything else. Endpoints with route-level middleware are left untouched.
   *
   * This trades a build-time `import` of each route module (guarded — any failure
   * silently falls back to the generic path) for faster per-request validation.
   */
  aot: Default(Boolean, false),
});

/**
 * Extra options for the api() factory that are not part of the Config schema.
 */
export interface ApiConfigExtras {
  scanPath?: string;
  /** First-party service identity, credential profiles, and default client resilience policy. */
  client?: ClientServiceContract;
  /**
   * Pre-loaded module to use instead of dynamic import in warmup.
   * Pass the already-imported generated `.api-application.gen.ts` module
   * to enable bundled builds where dynamic imports cannot be resolved.
   *
   * @example
   * ```typescript
   * import * as apiModule from './.gen/src/api/.api-application.gen.ts';
   * app.use(api({ preloadedModule: apiModule }));
   * ```
   */
  preloadedModule?: Record<string, unknown>;
}
