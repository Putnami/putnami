import { CacheMiddleware, type HttpCacheOptions, type HttpMiddleware } from '@putnami/application';
import type React from 'react';
import type { ClientSecurityRequirement } from '../shared/security.types';
import { withMiddleware } from './builder-mixin';

// ---------------------------------------------------------------------------
// LayoutDefinition — the object produced by layout().render(Component)
// ---------------------------------------------------------------------------

const LAYOUT_MARKER = 'putnami:layout' as const;

export interface LayoutDefinition {
  readonly __layout: typeof LAYOUT_MARKER;
  readonly component: React.ComponentType;
  readonly middleware: readonly HttpMiddleware[];
  /**
   * Declarative security requirement, populated when `.secure()` was called.
   * Surfaced for the SSR generator so it can attach it to the client route.
   */
  readonly security?: ClientSecurityRequirement;
}

export function isLayoutDefinition(value: unknown): value is LayoutDefinition {
  return typeof value === 'object' && value !== null && (value as LayoutDefinition).__layout === LAYOUT_MARKER;
}

// ---------------------------------------------------------------------------
// LayoutBuilder — fluent API built by layout()
// ---------------------------------------------------------------------------

class LayoutBuilderBase {
  _middleware: HttpMiddleware[] = [];
  _security?: ClientSecurityRequirement;
}

export class LayoutBuilder extends withMiddleware(LayoutBuilderBase) {
  /** Set cache headers (Cache-Control, ETag) for all pages under this layout */
  cache(options: HttpCacheOptions = {}): this {
    this._middleware.push(CacheMiddleware(options));
    return this;
  }

  /** Finalise the layout definition with the layout component */
  render(component: React.ComponentType): LayoutDefinition {
    return {
      __layout: LAYOUT_MARKER,
      component,
      middleware: [...this._middleware],
      ...(this._security ? { security: this._security } : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// layout() — entry point
// ---------------------------------------------------------------------------

/**
 * Declare a layout with its configuration (security, CORS, rate limiting, cache)
 * and component in a single file.
 *
 * Export the result as the default export of your `layout.tsx`.
 *
 * **Example — require authentication for an entire section:**
 * ```ts
 * // src/app/admin/layout.tsx
 * import { layout } from '@putnami/web';
 *
 * export default layout()
 *   .secure({ roles: ['admin'] })
 *   .render(function AdminLayout() {
 *     return <Outlet />;
 *   });
 * ```
 *
 * Layout middleware is composed with page middleware. Layout middleware runs
 * first (outermost), then page middleware, then the handler.
 */
export function layout(): LayoutBuilder {
  return new LayoutBuilder();
}
