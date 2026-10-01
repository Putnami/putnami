import type { HttpMiddleware } from '../http/http-middleware.type';
import { HttpResponse } from '../http/http-response';
import { AuthDecision, PrincipalKind } from './identity.constants';
import { recordDecision } from './security.observe';
import type { Principal, SecurityGuard, SecurityOptions } from './security.types';
import {
  type Claims,
  claimMatchesAudience,
  claimMatchesIssuer,
  hasAll,
  hasAny,
  resolveClientClaims,
  resolveRoles,
  resolveScopes,
  toArray,
} from './security.utils';

/**
 * Security middleware factory.
 *
 * Enforces access requirements on a handler. Assumes `ctx.user` is already
 * populated by the global identity resolver (added by the OAuth plugin).
 *
 * - No identity (`ctx.user` is `undefined`) → 401 Unauthorized, unless the
 *   options are `optional` and the `Authorization` header is absent or empty
 * - Identity present but insufficient permissions → 403 Forbidden
 * - All checks pass → proceeds to the handler
 *
 * Every allow/deny is logged and counted via {@link recordDecision}, keyed by
 * the {@link AuthDecision} label plus the failing dimension. The client only
 * ever receives a bare 401/403 body — the specific failing scope/role/client is
 * recorded server-side (logs + telemetry) and never leaked to the caller.
 *
 * @example Declarative options
 * ```ts
 * SecurityMiddleware({ roles: ['admin'], scopes: ['write'] })
 * ```
 *
 * @example Custom guard
 * ```ts
 * SecurityMiddleware((user, ctx) => user.orgId === ctx.params?.orgId)
 * ```
 */
export const SecurityMiddleware = (optionsOrGuard: SecurityOptions | SecurityGuard = {}): HttpMiddleware => {
  if (typeof optionsOrGuard === 'function') {
    return guardMiddleware(optionsOrGuard);
  }
  return optionsMiddleware(optionsOrGuard);
};

const guardMiddleware =
  (guard: SecurityGuard): HttpMiddleware =>
  async (ctx, next) => {
    if (!ctx.user) {
      recordDecision(ctx, AuthDecision.DenyUnauthenticated, 'no identity');
      return HttpResponse.unauthorized();
    }

    const allowed = await guard(ctx.user, ctx);
    if (!allowed) {
      recordDecision(ctx, AuthDecision.DenyGuard, 'guard rejected');
      return HttpResponse.forbidden();
    }

    recordDecision(ctx, AuthDecision.Allow, '');
    return next();
  };

/** Extract Bearer token from Authorization header. */
function extractBearerToken(ctx: { req: Request }): string | undefined {
  const header = ctx.req.headers.get('Authorization');
  return header?.startsWith('Bearer ') ? header.slice(7).trim() : undefined;
}

/**
 * An optional rule serves a request that resolved no identity only when its
 * `Authorization` header is absent or empty after trimming, because an empty
 * header presents no credential. A credential the resolvers refused is answered
 * 401, so a caller holding an expired or forged token learns it instead of
 * silently receiving the anonymous answer. The Go twin is `admitsAnonymous` in
 * go/framework/security/middleware.go.
 */
function admitsAnonymous(ctx: { req: Request }, options: SecurityOptions): boolean {
  return options.optional === true && (ctx.req.headers.get('Authorization') ?? '').trim() === '';
}

/**
 * Fail-closed check that the principal's kind is one of the required kinds. A
 * principal with no `kind` (unknown) never satisfies an explicit requirement.
 */
function principalKindMatches(user: Principal, required: SecurityOptions['principalKind']): boolean {
  const allowed = toArray(required);
  if (!allowed.length) return true;
  return typeof user.kind === 'string' && allowed.includes(user.kind);
}

const optionsMiddleware =
  (options: SecurityOptions): HttpMiddleware =>
  async (ctx, next) => {
    // When a custom verifier is provided, resolve identity inline
    if (options.verify) {
      const token = extractBearerToken(ctx);
      if (token) {
        const verified = await options.verify(token);
        if (!verified) {
          recordDecision(ctx, AuthDecision.DenyUnauthenticated, 'token verification failed');
          return HttpResponse.unauthorized();
        }
        // A custom verifier authenticates a bearer token; treat the resulting
        // principal as a `user` (default) when it declares no `kind`, so
        // `.secure({ verify, principalKind: 'user' })` is satisfiable — consistent
        // with the framework's bearer/introspection strategies.
        ctx.user = typeof verified['kind'] === 'string' ? verified : { ...verified, kind: PrincipalKind.User };
      } else if (admitsAnonymous(ctx, options)) {
        // The verifier is this route's only identity source: an identity another
        // resolver set (an API key, a session) was never verified here, so a
        // request without a bearer token is anonymous.
        ctx.user = undefined;
      } else {
        recordDecision(ctx, AuthDecision.DenyUnauthenticated, 'no bearer token');
        return HttpResponse.unauthorized();
      }
    }

    if (!ctx.user) {
      if (admitsAnonymous(ctx, options)) {
        recordDecision(ctx, AuthDecision.Allow, '');
        return next();
      }
      recordDecision(ctx, AuthDecision.DenyUnauthenticated, 'no identity');
      return HttpResponse.unauthorized();
    }

    const claims = ctx.user as Claims;

    // Fail-closed issuer/audience binding. A token whose `iss`/`aud` does not
    // match (or is absent) is rejected rather than silently trusted — otherwise
    // `.secure({ issuer, audience })` would be a fail-open no-op. Issuer/audience
    // are relying-party binding failures, recorded under `deny_client` (403).
    if (options.issuer && !claimMatchesIssuer(claims, options.issuer)) {
      recordDecision(ctx, AuthDecision.DenyClient, 'issuer mismatch');
      return HttpResponse.forbidden();
    }
    if (options.audience && !claimMatchesAudience(claims, options.audience)) {
      recordDecision(ctx, AuthDecision.DenyClient, 'audience mismatch');
      return HttpResponse.forbidden();
    }

    // A request whose principal kind (bearer user vs static api key) does not
    // match the requirement is denied with `deny_client`.
    if (options.principalKind && !principalKindMatches(ctx.user, options.principalKind)) {
      recordDecision(ctx, AuthDecision.DenyClient, 'principal kind not allowed');
      return HttpResponse.forbidden();
    }

    if (options.client) {
      const allowedClients = toArray(options.client);
      const tokenClients = resolveClientClaims(claims);
      if (!tokenClients.length || !tokenClients.some((client) => allowedClients.includes(client))) {
        recordDecision(ctx, AuthDecision.DenyClient, 'client not allowed');
        return HttpResponse.forbidden();
      }
    }

    const scopes = resolveScopes(claims, options);
    if (options.scopes?.length && !hasAll(scopes, options.scopes)) {
      recordDecision(ctx, AuthDecision.DenyScope, 'missing required scope');
      return HttpResponse.forbidden();
    }
    if (options.scopesAny?.length && !hasAny(scopes, options.scopesAny)) {
      recordDecision(ctx, AuthDecision.DenyScope, 'missing any scope');
      return HttpResponse.forbidden();
    }

    const roles = resolveRoles(claims, options);
    if (options.roles?.length && !hasAll(roles, options.roles)) {
      recordDecision(ctx, AuthDecision.DenyRole, 'missing required role');
      return HttpResponse.forbidden();
    }
    if (options.rolesAny?.length && !hasAny(roles, options.rolesAny)) {
      recordDecision(ctx, AuthDecision.DenyRole, 'missing any role');
      return HttpResponse.forbidden();
    }

    recordDecision(ctx, AuthDecision.Allow, '');
    return next();
  };
