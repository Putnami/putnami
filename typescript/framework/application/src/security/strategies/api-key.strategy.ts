import { timingSafeEqual } from 'node:crypto';
import { PrincipalKind } from '../identity.constants';
import type { Principal } from '../security.types';
import type { AuthStrategy } from './auth-strategy';

/** Configures the {@link apiKeyStrategy}. */
export interface ApiKeyStrategyOptions {
  /** The set of valid API keys. A request matching any one authenticates. */
  keys: string[];
  /** HTTP header the key is read from. Defaults to `X-Api-Key`. */
  header?: string;
  /** Subject claim set on the resolved principal. Defaults to `apikey`. */
  subject?: string;
  /** Scopes granted to API-key clients (space-joined into the `scope` claim). */
  scopes?: string[];
}

/**
 * Constant-time string comparison, reusing the `constantTimeEqual` precedent in
 * `src/http/csrf.middleware.ts`: it delegates to Node's `crypto.timingSafeEqual`
 * and, on a length mismatch, still runs a same-length comparison so there is no
 * early-exit fast path that leaks length by timing. This is the same primitive
 * Go uses (`crypto/subtle.ConstantTimeCompare`) — never a `===`/loop compare that
 * short-circuits on the first differing byte.
 */
function constantTimeEqual(a: string, b: string): boolean {
  const bufA = Buffer.from(a);
  const bufB = Buffer.from(b);
  if (bufA.length !== bufB.length) {
    timingSafeEqual(bufA, bufA);
    return false;
  }
  return timingSafeEqual(bufA, bufB);
}

/**
 * Authenticates a request by a static API key sent in an HTTP header, using a
 * constant-time comparison. Twin of `go/framework/security/apikey.go`.
 *
 * On a match it resolves a principal of kind {@link PrincipalKind.ApiKey}; a
 * missing header, or a key matching none of the configured keys, resolves nothing
 * (the strategy did not authenticate).
 *
 * @example
 * ```ts
 * authenticate({ anyOf: [apiKeyStrategy({ keys: [process.env.INGEST_KEY!], scopes: ['ingest'] })] })
 * ```
 */
export const apiKeyStrategy = (options: ApiKeyStrategyOptions): AuthStrategy => {
  const header = options.header || 'X-Api-Key';
  const subject = options.subject || PrincipalKind.ApiKey;
  const scopes = options.scopes ?? [];
  const keys = options.keys ?? [];

  return (ctx) => {
    const presented = ctx.req.headers.get(header);
    if (!presented) {
      return undefined;
    }

    // Compare against every configured key WITHOUT short-circuiting on the first
    // match, so the timing does not reveal which key (or how many) matched —
    // mirrors the `subtle.ConstantTimeCompare` loop in apikey.go.
    let matched = false;
    for (const key of keys) {
      if (constantTimeEqual(presented, key)) {
        matched = true;
      }
    }
    if (!matched) {
      return undefined;
    }

    const principal: Principal = {
      sub: subject,
      client_id: 'apikey-client',
      kind: PrincipalKind.ApiKey,
    };
    if (scopes.length > 0) {
      principal.scope = scopes.join(' ');
    }
    return principal;
  };
};
