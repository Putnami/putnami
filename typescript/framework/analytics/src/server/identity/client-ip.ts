import { buildTrustedProxyMatcher, type HttpRequestContext, trustedProxyKeyGenerator } from '@putnami/application';

/**
 * Builds the client-IP resolver used to key the visitor hash and the ingest
 * rate limit (body §D.3).
 *
 * It reuses the application's trusted-proxy semantics rather than reading
 * `X-Forwarded-For` here: with an empty trusted list the key is the socket
 * peer and the header is never honoured, so a visitor cannot forge a key — or
 * a rate-limit bucket — by setting a header. The returned value is a keying
 * input only; nothing stores it.
 *
 * @param trustedProxies - Exact IPs and CIDR ranges allowed to forward.
 * @returns A resolver from a request context to the client key.
 */
export function createClientIpResolver(
  trustedProxies: readonly string[] | undefined,
): (ctx: HttpRequestContext) => string {
  const generator = trustedProxyKeyGenerator(buildTrustedProxyMatcher(trustedProxies ?? []));
  return (ctx: HttpRequestContext): string => generator({ req: ctx.req, headers: ctx.headers, server: ctx.server });
}
