import type { HttpMiddleware } from '@putnami/application';
import type React from 'react';
import type { ClientSecurityRequirement } from '../shared/security.types';
import { withMiddleware } from './builder-mixin';

// ---------------------------------------------------------------------------
// NotFoundDefinition — the object produced by notFound().render(Component)
// ---------------------------------------------------------------------------

const NOT_FOUND_MARKER = 'putnami:not-found' as const;

export interface NotFoundDefinition {
  readonly __notFound: typeof NOT_FOUND_MARKER;
  readonly component: React.ComponentType;
  readonly middleware: readonly HttpMiddleware[];
  /**
   * Declarative security requirement, populated when `.secure()` was called.
   * Surfaced for the SSR generator so it can attach it to the client route.
   */
  readonly security?: ClientSecurityRequirement;
}

export function isNotFoundDefinition(value: unknown): value is NotFoundDefinition {
  return typeof value === 'object' && value !== null && (value as NotFoundDefinition).__notFound === NOT_FOUND_MARKER;
}

// ---------------------------------------------------------------------------
// NotFoundBuilder — fluent API built by notFound()
// ---------------------------------------------------------------------------

class NotFoundBuilderBase {
  _middleware: HttpMiddleware[] = [];
  _security?: ClientSecurityRequirement;
}

export class NotFoundBuilder extends withMiddleware(NotFoundBuilderBase) {
  /** Finalise the not-found definition with the not-found component */
  render(component: React.ComponentType): NotFoundDefinition {
    return {
      __notFound: NOT_FOUND_MARKER,
      component,
      middleware: [...this._middleware],
      ...(this._security ? { security: this._security } : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// notFound() — entry point
// ---------------------------------------------------------------------------

/**
 * Declare a not-found page with its configuration (security, CORS, rate limiting)
 * and component in a single file.
 *
 * Export the result as the default export of your `not-found.tsx`.
 *
 * **Example — rate limit 404 pages:**
 * ```ts
 * // src/app/not-found.tsx
 * import { notFound } from '@putnami/web';
 *
 * export default notFound()
 *   .rateLimit({ max: 50 })
 *   .render(function NotFoundPage() {
 *     return <h1>Page not found</h1>;
 *   });
 * ```
 */
export function notFound(): NotFoundBuilder {
  return new NotFoundBuilder();
}
