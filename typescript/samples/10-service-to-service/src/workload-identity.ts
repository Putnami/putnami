import { authenticate, type HttpMiddleware, type Principal } from '@putnami/application';
import { CALLER_SCOPE, SAMPLE_TENANT, TENANT_HEADER, TENANT_SCOPE, USER_SUBJECTS } from './caller-identity';

/** Header the `catalog-key` credential profile carries. */
export const CATALOG_KEY_HEADER = 'X-Catalog-Key';

/** The API key the provider accepts for the watch stream. */
export const CATALOG_API_KEY = 'sample-catalog-key';

/** A key that authenticates but carries none of the scopes the stream requires. */
export const CATALOG_API_KEY_WITHOUT_SCOPE = 'sample-catalog-key-no-scope';

/** Scope the watch stream requires. */
export const WATCH_SCOPE = 'catalog.watch';

/**
 * Resolve the caller identity from the API key the `catalog-key` credential
 * profile carries. It runs before any route, so an endpoint declares only what
 * it requires (`.secure({ scopes })`) and never how identity is established —
 * which is what keeps the endpoint representable in the first-party client
 * contract, where a custom verifier is refused.
 *
 * No identity gives 401 and a missing scope gives 403, both decided before the
 * response head: a consumer learns a stream was refused from the status and
 * never from an event. A real provider would look the key up in its own secret
 * store; the sample keeps the shape and skips the storage, exactly like the Go
 * sample's IdentityResolver.
 */
export function catalogIdentityResolver(): HttpMiddleware {
  // Composed with `authenticate` rather than hand-rolled: that is what
  // registers the resolver on the request, and a first-party WebSocket replays
  // the registered resolvers against the request it rebuilds from the init
  // frame. A hand-rolled middleware only ever sees the bare upgrade, which
  // carries no credential by design, so every conforming client would be
  // refused with 401 before its first message.
  return authenticate({
    anyOf: [
      // The api key and the tenant together carry more than the key alone, so
      // this resolver is asked first: `anyOf` takes the first principal it gets.
      (ctx) => principalForTenant(ctx.req.headers.get(CATALOG_KEY_HEADER), ctx.req.headers.get(TENANT_HEADER)),
      (ctx) => principalForKey(ctx.req.headers.get(CATALOG_KEY_HEADER)),
      // A user token a consumer forwarded names that user rather than the
      // calling workload, and carries the scope /whoami requires.
      (ctx) => principalForUser(ctx.req.headers.get('authorization')),
    ],
  });
}

function principalForTenant(key: string | null, tenant: string | null): Principal | undefined {
  if (key !== CATALOG_API_KEY || tenant !== SAMPLE_TENANT) return undefined;
  return {
    sub: 'sample-workload',
    client_id: 'service-to-service-sample',
    scope: `${WATCH_SCOPE} ${TENANT_SCOPE}`,
  };
}

function principalForUser(authorization: string | null): Principal | undefined {
  const subject = USER_SUBJECTS[(authorization ?? '').replace(/^Bearer /, '')];
  return subject ? { sub: subject, client_id: 'service-to-service-sample', scope: CALLER_SCOPE } : undefined;
}

function principalForKey(key: string | null): Principal | undefined {
  if (key === CATALOG_API_KEY) {
    return { sub: 'sample-workload', client_id: 'service-to-service-sample', scope: WATCH_SCOPE };
  }
  if (key === CATALOG_API_KEY_WITHOUT_SCOPE) {
    return { sub: 'sample-workload', client_id: 'service-to-service-sample', scope: '' };
  }
  return undefined;
}
