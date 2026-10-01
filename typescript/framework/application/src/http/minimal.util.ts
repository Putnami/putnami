import type { HttpRequestContext, HttpRequestContextInternal } from './http-context.type';

/**
 * Whether the matched route is a minimal / fast-lane route the always-on security
 * middleware (origin guard, security headers) should skip.
 *
 * The security middleware runs once, before the dispatcher has chosen which of
 * the matched candidates actually produces the response. So we only skip when
 * **every** matched candidate is minimal — otherwise a non-minimal handler
 * reached through fallthrough (a higher-scored candidate returning `undefined`
 * or throwing `NotAcceptable`) would run with the security defaults already
 * skipped, leaking the opt-out across routes.
 */
export function isMinimalRoute(ctx: HttpRequestContext): boolean {
  const matched = (ctx as HttpRequestContextInternal).__matchedHandlers;
  return matched !== undefined && matched.length > 0 && matched.every((h) => h.minimal === true);
}

/**
 * Whether the matched route opted out of secure response headers only.
 *
 * Like {@link isMinimalRoute}, this only skips when every matched candidate has
 * opted out; otherwise fallthrough could leak the opt-out onto a later handler.
 */
export function skipsSecurityHeaders(ctx: HttpRequestContext): boolean {
  const matched = (ctx as HttpRequestContextInternal).__matchedHandlers;
  return (
    matched !== undefined &&
    matched.length > 0 &&
    matched.every((h) => h.minimal === true || h.securityHeaders === false)
  );
}
