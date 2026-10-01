import type { HttpMiddleware } from '@putnami/application';
import { withMiddleware } from './builder-mixin';

// ---------------------------------------------------------------------------
// MiddlewareDefinition — the object produced by middleware()
// ---------------------------------------------------------------------------

const MIDDLEWARE_MARKER = 'putnami:middleware' as const;

export interface MiddlewareDefinition {
  readonly __middleware: typeof MIDDLEWARE_MARKER;
  readonly handler: HttpMiddleware;
  readonly stack: readonly HttpMiddleware[];
}

export function isMiddlewareDefinition(value: unknown): value is MiddlewareDefinition {
  return (
    typeof value === 'object' && value !== null && (value as MiddlewareDefinition).__middleware === MIDDLEWARE_MARKER
  );
}

// ---------------------------------------------------------------------------
// MiddlewareBuilder — fluent API built by middleware()
// ---------------------------------------------------------------------------

class MiddlewareBuilderBase {
  _middleware: HttpMiddleware[] = [];
}

export class MiddlewareBuilder extends withMiddleware(MiddlewareBuilderBase) {
  /** Finalise and return the composed middleware definition */
  build(): MiddlewareDefinition {
    const stack = [...this._middleware];

    // Compose all middleware into a single HttpMiddleware
    const handler: HttpMiddleware = async (ctx, next) => {
      const chain = stack.reduceRight<() => ReturnType<typeof next>>((nextFn, mw) => async () => mw(ctx, nextFn), next);
      return chain();
    };

    return {
      __middleware: MIDDLEWARE_MARKER,
      handler,
      stack,
    };
  }
}

// ---------------------------------------------------------------------------
// middleware() — entry point
// ---------------------------------------------------------------------------

/**
 * Compose multiple middleware into a single reusable middleware definition.
 *
 * **Example — compose auth + rate limiting:**
 * ```ts
 * import { middleware } from '@putnami/web';
 *
 * export const apiGuard = middleware()
 *   .secure({ roles: ['user'] })
 *   .rateLimit({ max: 100 })
 *   .build();
 *
 * // Use in page or layout configs:
 * export default page()
 *   .use(apiGuard.handler)
 *   .render(MyPage);
 * ```
 */
export function middleware(): MiddlewareBuilder {
  return new MiddlewareBuilder();
}
