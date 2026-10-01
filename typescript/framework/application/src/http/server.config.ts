import type { CompressionOptions } from './compression.middleware';
import type { CsrfOptions } from './csrf.middleware';
import type { ErrorHandler } from './http-dispatcher';
import type { HttpMethodsOptions } from './http-methods.middleware';
import type { OriginGuardOptions } from './origin-guard.middleware';
import type { SecurityHeadersOptions } from './security-headers.middleware';

export interface ServerOptions {
  port?: number;
  /**
   * Secure-by-default master switch. `true` (the default) keeps the always-on
   * security middleware: the origin guard, the secure response headers, and the
   * automatic HEAD/OPTIONS handling.
   *
   * Set to `false` for a **minimal / unsecured fast lane** — an explicit, logged
   * opt-out (a `⚠️ security middleware disabled` line is printed at startup).
   * The security defaults are skipped, which — combined with the empty-chain
   * bypass and the lazy DI scope — removes the per-request security tax for
   * trusted, unauthenticated hot paths.
   *
   * Individually-set options (`originGuard`, `securityHeaders`, `httpMethods`)
   * still take precedence, so you can re-enable one piece while opting out of the
   * rest. The same opt-out is available per route via `{ minimal: true }` /
   * `endpoint().minimal()`.
   *
   * This approaches but does **not** reach bare `Bun.serve` throughput; it is a
   * complement to per-route AOT, not a substitute. Keep it `true` unless a route
   * is genuinely trusted and unauthenticated.
   */
  secure?: boolean;
  /**
   * Maximum request body size in bytes.
   *
   * Default: `1_048_576` (1 MiB).
   * Set to `0` to disable body size checks.
   */
  maxBodySizeBytes?: number;
  /**
   * Request timeout in milliseconds.
   *
   * Default: `30_000` (30 seconds).
   * Set to `0` to disable request timeouts.
   */
  requestTimeoutMs?: number;
  /**
   * Automatically handle HEAD, OPTIONS, and TRACE HTTP methods.
   *
   * - `head` (default `true`) — derive HEAD responses from GET handlers.
   * - `options` (default `true`) — respond with `Allow` header for registered routes.
   * - `trace` (default `false`) — echo requests back (disabled by default for security).
   *
   * Set to `false` to disable all automatic method handling.
   */
  httpMethods?: HttpMethodsOptions | false;
  /**
   * Origin-based CSRF protection using `Origin` and `Sec-Fetch-Site` headers.
   *
   * Enabled by default. On state-changing requests (POST, PUT, DELETE, PATCH)
   * the server validates that the request originates from the same origin.
   *
   * Set to `false` to disable entirely.
   */
  originGuard?: OriginGuardOptions | false;
  /**
   * Token-based CSRF protection (double-submit cookie).
   *
   * Disabled by default. When enabled, the server establishes a `_csrf` cookie
   * when a matched safe request has no valid token, reuses it on subsequent
   * requests, and validates the `X-CSRF-Token` header on state-changing requests.
   * Client-side form actions and `useFetch` inject the token automatically.
   *
   * Set to `true` for defaults, or pass `CsrfOptions` to customise.
   *
   * **Note:** `api()` routes are exempt from this token check by default (the
   * same-origin `originGuard` still applies). Opt an API plugin into token
   * validation with `api({ csrf: true })`.
   */
  csrf?: CsrfOptions | boolean;
  /**
   * Global error handler invoked when a route handler throws.
   *
   * Return a value to use as the response body (will be negotiated),
   * or `undefined` to fall through to the default error handling.
   *
   * @example
   * ```typescript
   * http({
   *   onError: (error, ctx) => {
   *     reportToSentry(error);
   *     return { message: 'Something went wrong', traceId: ctx.traceId };
   *   },
   * })
   * ```
   */
  onError?: ErrorHandler;
  /**
   * Enable response compression (gzip/deflate) for compressible content types.
   *
   * Disabled by default. When enabled, the server compresses text-based
   * responses (HTML, JSON, CSS, JS, SVG) that exceed the size threshold.
   * Already-encoded responses (e.g. pre-compressed static files) are skipped.
   *
   * Set to `true` for defaults, or pass `CompressionOptions` to customise.
   */
  compression?: CompressionOptions | boolean;
  /**
   * WebSocket resource bounds applied to Bun's websocket handler.
   *
   * Defaults bound client-controlled memory use (DoS): inbound frames larger
   * than `maxPayloadLength` are rejected by the runtime, and a connection whose
   * buffered outbound data exceeds `backpressureLimit` is closed.
   */
  webSocket?: {
    /** Max inbound message size in bytes. Default `1_048_576` (1 MiB). */
    maxPayloadLength?: number;
    /** Max buffered outbound bytes before the connection is closed. Default `16_777_216` (16 MiB). */
    backpressureLimit?: number;
  };
  /**
   * Secure-by-default response headers (`X-Content-Type-Options: nosniff`,
   * `X-Frame-Options`, `Content-Security-Policy`, `Referrer-Policy`,
   * `Permissions-Policy`, `Strict-Transport-Security`, `X-XSS-Protection`).
   *
   * Enabled by default with hardened values applied to every response. Pass
   * `SecurityHeadersOptions` to override individual headers (or set a header to
   * `false` to omit it), or set to `false` to disable the headers entirely.
   */
  securityHeaders?: SecurityHeadersOptions | false;
}
