import type { HttpMiddleware } from '../http/http-middleware.type';
import { HttpResponse } from '../http/http-response';

/** Header used by @putnami/client to declare client identity. */
const CLIENT_ID_HEADER = 'X-Client-Id';

/**
 * HTTP middleware that restricts access to specific client services.
 *
 * Checks the `X-Client-Id` header against an allowlist. This is the receiving side
 * counterpart to the `clientId` option in `@putnami/client`'s `ClientConfig`.
 *
 * For cryptographic client verification, combine with `.secure({ client: [...] })`
 * which validates the `azp`/`client_id` claims in the JWT.
 *
 * @example
 * ```typescript
 * import { requireClient } from '@putnami/application';
 *
 * // Only allow these services to call this endpoint
 * app.post('/internal/sync', requireClient(['orders-service', 'billing-service']), handler);
 *
 * // Combine with JWT auth for two-layer verification:
 * // 1. JWT proves the caller is authenticated (cryptographic)
 * // 2. X-Client-Id declares which service is calling (identity)
 * app.post('/internal/sync',
 *   SecurityMiddleware({ client: ['orders-service'] }),
 *   requireClient(['orders-service']),
 *   handler,
 * );
 * ```
 */
export function requireClient(allowedClients: string[]): HttpMiddleware {
  const allowedSet = new Set(allowedClients);

  return async (ctx, next) => {
    const clientId = ctx.headers.get(CLIENT_ID_HEADER);

    if (!clientId || !allowedSet.has(clientId)) {
      return HttpResponse.forbidden();
    }

    return next();
  };
}
