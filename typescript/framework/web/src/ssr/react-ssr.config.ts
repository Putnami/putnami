import { ArrayOf, Config, Default, Int, Optional, shouldExposeErrorStack } from '@putnami/runtime';
import type { ReactElement, ReactNode } from 'react';

export const PutnamiReactConfig = Config('putnami.react', {
  scanFolder: Default(String, 'app'),
  scanPath: Optional(String),
  scanRoots: Optional(
    ArrayOf({
      path: String,
      routePrefix: Optional(String),
    }),
  ),
  autoScan: Default(Boolean, true),
  buildEnable: Default(Boolean, true),
  sourcemap: Default(String, 'external'),
  minify: Default(Boolean, true),
  splitting: Default(Boolean, false),
  ssrTimeout: Default(Int, 3000),
  /** Timeout (ms) for client-side loader/action data fetches. */
  clientFetchTimeout: Default(Int, 30_000),
  /** Maximum entries in the SSR module and page-data LRU caches before eviction. */
  moduleCacheSize: Default(Int, 500),
  scriptsFolder: Default(String, 'scripts'),
  /**
   * Whether `handleRenderError` may put the raw error into the 500 page.
   *
   * The default is read at module load through {@link shouldExposeErrorStack},
   * not written as `process.env.NODE_ENV !== 'production'`: the bundler folds
   * that expression to `true` when this package is published, so the
   * consumer's `NODE_ENV` would never reach the default.
   */
  isDevelopment: Default(Boolean, shouldExposeErrorStack()),
  /**
   * Emit secure-by-default response headers (Content-Security-Policy with the
   * per-request hydration nonce, X-Content-Type-Options: nosniff,
   * Referrer-Policy, etc.) on SSR responses. Set to `false` to opt out, e.g.
   * when you register your own `securityHeaders()` plugin or a reverse proxy
   * adds them. Default: `true`.
   */
  securityHeaders: Default(Boolean, true),
  /**
   * Validate CSRF tokens on `action()` POST endpoints (double-submit cookie
   * via CsrfMiddleware). Page GETs issue the `_csrf` cookie; unsafe requests
   * must echo it back via the `X-CSRF-Token` header (done automatically by the
   * client action handler) or a `_csrf` form field (`<CsrfInput />`), or they
   * are rejected with 403. Set to `false` to opt out, e.g. when a reverse
   * proxy or your own CsrfMiddleware handles CSRF. Default: `true`.
   */
  csrf: Default(Boolean, true),
});

export interface PutnamiReactConfigExtras {
  page?: ({ children }: { children?: ReactNode }) => ReactElement;
  notFound?: ({ children }: { children?: ReactNode }) => ReactElement;
  /**
   * Pre-loaded module to use instead of dynamic import in warmup.
   * Pass the already-imported generated `.react-application.gen.tsx` module
   * to enable bundled builds where dynamic imports cannot be resolved.
   *
   * @example
   * ```typescript
   * import * as reactModule from './.gen/src/app/.react-application.gen.tsx';
   * app.use(react({ preloadedModule: reactModule }));
   * ```
   */
  preloadedModule?: Record<string, unknown>;
}
