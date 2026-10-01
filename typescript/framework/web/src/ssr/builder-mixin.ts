import {
  CorsMiddleware,
  type CorsOptions,
  type HttpMiddleware,
  RateLimitMiddleware,
  type RateLimitOptions,
  SecurityMiddleware,
  type SecurityGuard,
  type SecurityOptions,
} from '@putnami/application';
import { normalizeClientSecurity } from '../shared/security-builder';
import type { ClientSecurityRequirement } from '../shared/security.types';

// biome-ignore lint/suspicious/noExplicitAny: TS requires a literal `any[]` rest parameter to recognize this as a mixin constructor
type Constructor<T = object> = new (...args: any[]) => T;

/**
 * Mixin that adds the route-builder methods shared between
 * `page()`, `layout()`, `error()`, and `notFound()`.
 *
 * The base class must expose:
 * - `_middleware: HttpMiddleware[]` — the chain of HTTP middleware to apply.
 * - `_security?: ClientSecurityRequirement` — the declarative requirement
 *   surfaced on the final definition so the client React-Router generator
 *   can attach it to `route.handle.security`.
 *
 * Method parity with the browser-side `ClientBuilder` is intentional:
 * shared `page.tsx` modules are compiled into both bundles, so any method
 * that exists on one builder must exist on the other.
 *
 * @example
 * ```ts
 * class MyBuilderBase {
 *   _middleware: HttpMiddleware[] = [];
 *   _security?: ClientSecurityRequirement;
 * }
 * class MyBuilder extends withMiddleware(MyBuilderBase) { ... }
 * ```
 */
export function withMiddleware<
  TBase extends Constructor<{
    _middleware: HttpMiddleware[];
    _security?: ClientSecurityRequirement;
  }>,
>(Base: TBase) {
  return class extends Base {
    /** Enable CORS for this route with the given options. */
    cors(options: CorsOptions = {}): this {
      this._middleware.push(CorsMiddleware(options));
      return this;
    }

    /** Apply rate limiting. */
    rateLimit(options: RateLimitOptions = {}): this {
      this._middleware.push(RateLimitMiddleware(options));
      return this;
    }

    /**
     * Require authentication and enforce access rules.
     *
     * Pushes a server-side `SecurityMiddleware` AND captures the declarative
     * requirement so the SSR generator can forward it to React Router's
     * `handle.security` for client-side gating (e.g. hiding admin links).
     * JWT-specific fields are stripped from the client metadata; the
     * server retains the full options object for enforcement.
     */
    secure(optionsOrGuard?: SecurityOptions | SecurityGuard): this {
      this._middleware.push(SecurityMiddleware(optionsOrGuard ?? {}));
      this._security = normalizeClientSecurity(optionsOrGuard);
      return this;
    }

    /** Add a custom middleware. */
    use(middleware: HttpMiddleware): this {
      this._middleware.push(middleware);
      return this;
    }
  };
}
