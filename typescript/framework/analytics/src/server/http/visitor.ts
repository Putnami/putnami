import type { HttpRequestContext, HttpResponse } from '@putnami/application';
import { classifyUserAgent } from '../enrich/user-agent';
import { pushPendingCookie, resolveIdentifiedVisitor, takePendingCookies } from '../identity/identified-cookie';
import { visitorHash } from '../identity/visitor-hash';
import type { AnalyticsRuntime } from '../runtime';

/** The request slot the resolved visitor is memoized in. */
export const ANALYTICS_VISITOR_SLOT = '__putnamiAnalyticsVisitor';

/** Who the request is, in the closed vocabulary of the protocol. */
export interface VisitorIdentity {
  /** The visitor id stored on the row. */
  visitorId: string;
  /** How it was derived: a daily rotating hash, or a consented cookie. */
  visitorKind: 'daily' | 'cookie';
}

/**
 * Resolves the visitor of a request, once (body §D.1, §D.2).
 *
 * The result is memoized on the request context because the page-view
 * middleware and the ingest handler both need it on the same request: a second
 * derivation would re-run the consent callback and could mint a second cookie
 * for one visitor.
 *
 * A minted cookie is queued on the pending-cookie slot rather than returned,
 * so whichever layer owns the response attaches it exactly once — and so
 * `forget()`, which writes to the same slot from application code, reaches the
 * response through the same path.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @param ua - The `User-Agent` header, already read by the caller.
 * @returns The visitor id and how it was derived.
 */
export async function resolveVisitor(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  ua: string | null,
): Promise<VisitorIdentity> {
  const slots = ctx as unknown as Record<string, unknown>;
  const memoized = slots[ANALYTICS_VISITOR_SLOT] as VisitorIdentity | undefined;
  if (memoized) {
    return memoized;
  }
  const identity = await deriveVisitor(ctx, rt, ua);
  slots[ANALYTICS_VISITOR_SLOT] = identity;
  return identity;
}

async function deriveVisitor(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  ua: string | null,
): Promise<VisitorIdentity> {
  const identified = await resolveIdentifiedVisitor(ctx, rt);
  if (identified) {
    if (identified.setCookie) {
      pushPendingCookie(ctx, identified.setCookie);
    }
    return { visitorId: identified.visitorId, visitorKind: 'cookie' };
  }
  const { browser, os } = classifyUserAgent(ua);
  return {
    visitorId: visitorHash({
      secret: rt.secret,
      ts: rt.now(),
      app: rt.app,
      // The address is consumed inside the hash and never returned.
      clientIp: rt.clientIp(ctx),
      uaFamily: `${browser}/${os}`,
    }),
    visitorKind: 'daily',
  };
}

/**
 * Drains the pending `Set-Cookie` values onto a response.
 *
 * @param ctx - The request context.
 * @param res - The response to decorate.
 * @returns The same response, carrying every queued cookie.
 */
export function attachPendingCookies(ctx: HttpRequestContext, res: HttpResponse): HttpResponse {
  let decorated = res;
  for (const value of takePendingCookies(ctx)) {
    decorated = decorated.pushHeader('Set-Cookie', value);
  }
  return decorated;
}
