import { useLogger } from '@putnami/runtime';
import type { HttpMiddleware } from '../http/http-middleware.type';
import type { HttpRequestContext, HttpRequestContextInternal } from '../http/http-context.type';
import { useOAuthService } from '../oauth/oauth.service';
import { accessToken } from '../oauth/oauth.utils';
import { PrincipalKind } from './identity.constants';
import type { Principal } from './security.types';
import type { AuthStrategy } from './strategies/auth-strategy';
import type { Claims } from './security.utils';
import { toNonEmptyTuple } from './security.utils';

export interface IdentityResolverOptions {
  /** Expected token issuer(s). When set, tokens from other issuers are rejected. */
  issuer?: string | string[];
  /** Expected token audience(s). When set, tokens for other audiences are rejected. */
  audience?: string | string[];
}

/**
 * Global identity resolution middleware.
 *
 * Runs on every request to populate `ctx.user` with the authenticated
 * user's claims when a valid token or session exists.
 *
 * This middleware never rejects a request — if no valid token is found,
 * `ctx.user` remains `undefined` and the request proceeds normally.
 *
 * Token resolution order:
 * 1. `Authorization: Bearer <token>` header
 * 2. Session-based access token (with automatic refresh)
 *
 * @param options - Optional issuer/audience constraints for token verification
 *
 * @example
 * ```ts
 * // Registered globally by OAuthPlugin during warmup
 * httpPlugin.prepend(IdentityResolverMiddleware());
 *
 * // With issuer/audience validation
 * httpPlugin.prepend(IdentityResolverMiddleware({
 *   issuer: 'https://auth.example.com',
 *   audience: 'my-api',
 * }));
 * ```
 */
export const IdentityResolverMiddleware = (options?: IdentityResolverOptions): HttpMiddleware => {
  if (!options?.audience) {
    useLogger('oauth').warn(
      'IdentityResolverMiddleware is verifying access tokens without an audience constraint: any ' +
        'token signed by the trusted issuer is accepted regardless of its `aud` claim (token-confusion / ' +
        'wrong-audience risk). Set oauth.audience (or pass { audience }) to bind tokens to this API.',
    );
  }

  const middleware: HttpMiddleware = async (ctx, next) => {
    captureIdentityResolver(ctx, middleware);
    const header = ctx.req.headers.get('Authorization') ?? ctx.req.headers.get('authorization');
    let token = header?.startsWith('Bearer ') ? header.slice('Bearer '.length).trim() : undefined;

    if (!token) {
      try {
        token = await accessToken({ redirect: false });
      } catch {
        // Session not available or other error — proceed without identity
      }
    }

    if (token) {
      try {
        const oauthService = useOAuthService();
        const verifyOptions: Record<string, unknown> = {};
        if (options?.issuer) verifyOptions['issuer'] = toNonEmptyTuple(options.issuer);
        if (options?.audience) verifyOptions['audience'] = toNonEmptyTuple(options.audience);
        const claims = await oauthService.verify<Claims>(token, verifyOptions);
        if (claims) {
          // A verified bearer/session token is a `user` principal in the
          // PrincipalKind vocabulary (default; a `kind` claim in the token
          // overrides), so `.secure({ principalKind: 'user' })` is satisfiable
          // via the default OAuth resolver — matching the composable strategies.
          ctx.user = { kind: PrincipalKind.User, ...(claims as Record<string, unknown>) } as Principal;
        }
      } catch {
        // Verification failed — proceed without identity
      }
    }

    return next();
  };
  return middleware;
};

/**
 * Combinator options for {@link authenticate}. Exactly one of `anyOf` / `allOf`
 * must be supplied; combining them is rejected because the composed semantics
 * would be ambiguous.
 */
export interface AuthenticateOptions {
  /** First strategy that resolves a principal wins (short-circuits the rest). */
  anyOf?: AuthStrategy[];
  /**
   * Every strategy must resolve a principal; the results are merged into one.
   * If any strategy resolves nothing, the whole check resolves nothing (no
   * identity is set), so a downstream `.secure()` fails closed with 401.
   */
  allOf?: AuthStrategy[];
}

/**
 * Composes {@link AuthStrategy} functions into a single identity-resolver
 * middleware that produces ONE typed {@link Principal} on `ctx.user`.
 *
 * Like every resolver it is first-win at the chain level: if an earlier resolver
 * already set `ctx.user`, this middleware is a no-op. It never rejects a request
 * — an unauthenticated request simply proceeds with `ctx.user` unset, leaving
 * authorization to `.secure()`.
 *
 * - `anyOf`: strategies run in declaration order and the first that resolves a
 *   principal wins; later strategies (and their network calls) are skipped.
 * - `allOf`: strategies run in declaration order and EVERY one must resolve a
 *   principal. The resolved principals are merged into one in declaration order,
 *   last-write-wins on a claim-key collision (`{ ...s0, ...s1, ... }`). The merge
 *   is by array position, never by async completion order, so the resulting
 *   principal is deterministic. If any strategy resolves nothing the check fails
 *   closed (no `ctx.user`).
 *
 * A strategy that throws is treated as "did not authenticate" (fail closed).
 *
 * @example
 * ```ts
 * // Accept either a bearer JWT or an API key.
 * httpPlugin.prepend(authenticate({ anyOf: [bearerJwtStrategy(), apiKeyStrategy({ keys })] }));
 *
 * // Require BOTH an API key and a bearer JWT, merged into one principal.
 * httpPlugin.prepend(authenticate({ allOf: [apiKeyStrategy({ keys }), bearerJwtStrategy()] }));
 * ```
 */
export const authenticate = (options: AuthenticateOptions): HttpMiddleware => {
  const anyOf = options.anyOf ?? [];
  const allOf = options.allOf ?? [];
  if (anyOf.length === 0 && allOf.length === 0) {
    throw new Error('authenticate() requires at least one strategy in anyOf or allOf');
  }
  if (anyOf.length > 0 && allOf.length > 0) {
    throw new Error('authenticate() accepts either anyOf or allOf, not both');
  }

  const middleware: HttpMiddleware = async (ctx, next) => {
    captureIdentityResolver(ctx, middleware);
    // First-win at the chain level: never re-resolve an already-authenticated request.
    if (ctx.user) {
      return next();
    }
    const principal = anyOf.length > 0 ? await resolveAnyOf(ctx, anyOf) : await resolveAllOf(ctx, allOf);
    if (principal) {
      ctx.user = principal;
    }
    return next();
  };
  return middleware;
};

function captureIdentityResolver(context: HttpRequestContext, resolver: HttpMiddleware): void {
  const internal = context as HttpRequestContextInternal;
  internal.__identityResolvers ??= [];
  if (!internal.__identityResolvers.includes(resolver)) internal.__identityResolvers.push(resolver);
}

/** Runs a strategy and folds any thrown error into "did not authenticate". */
async function runStrategy(ctx: HttpRequestContext, strategy: AuthStrategy): Promise<Principal | undefined> {
  try {
    return (await strategy(ctx)) ?? undefined;
  } catch {
    return undefined;
  }
}

async function resolveAnyOf(ctx: HttpRequestContext, strategies: AuthStrategy[]): Promise<Principal | undefined> {
  for (const strategy of strategies) {
    const principal = await runStrategy(ctx, strategy);
    if (principal) {
      return principal;
    }
  }
  return undefined;
}

async function resolveAllOf(ctx: HttpRequestContext, strategies: AuthStrategy[]): Promise<Principal | undefined> {
  let merged: Principal = {};
  for (const strategy of strategies) {
    const principal = await runStrategy(ctx, strategy);
    if (!principal) {
      return undefined; // allOf: every strategy must authenticate
    }
    // Merge in declaration order; a later strategy wins on a claim-key collision.
    merged = { ...merged, ...principal };
  }
  return merged;
}
