import type { HttpRequestContext } from '../../http/http-context.type';
import type { Principal } from '../security.types';

/**
 * A composable authentication strategy.
 *
 * Given a request context, a strategy returns the authenticated {@link Principal}
 * it resolved, or `undefined` when it did not authenticate the request — either
 * because no credential of its kind was present, or the credential it found was
 * invalid. `undefined` is the "this strategy did not authenticate" signal the
 * {@link authenticate} combinators branch on; a strategy must therefore fail
 * closed (return `undefined`) rather than throw for an unauthenticated request.
 *
 * Strategies may be async (JWKS verification and RFC 7662 introspection are
 * network calls).
 */
export type AuthStrategy = (ctx: HttpRequestContext) => Principal | undefined | Promise<Principal | undefined>;

/**
 * Extracts the token from an `Authorization: Bearer <token>` header, or
 * `undefined` when the header is absent or not a bearer credential. Header names
 * are case-insensitive, so both `Authorization` and `authorization` are honored.
 */
export function extractBearerToken(ctx: HttpRequestContext): string | undefined {
  const header = ctx.req.headers.get('Authorization') ?? ctx.req.headers.get('authorization');
  if (!header?.startsWith('Bearer ')) {
    return undefined;
  }
  return header.slice('Bearer '.length).trim() || undefined;
}
