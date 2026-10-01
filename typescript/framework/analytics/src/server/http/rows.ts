import type { HttpRequestContext } from '@putnami/application';
import { classifyUserAgent } from '../enrich/user-agent';
import type { AnalyticsRuntime } from '../runtime';
import { LANGUAGE_RE } from '../sanitize/vocabulary';
import { utcDay } from '../identity/visitor-hash';
import type { EventRow } from '../sink/fold';
import type { VisitorIdentity } from './visitor';

/** A server view whose request matched no route (body §B.1). */
export const ROUTE_UNMATCHED = '__unmatched__';

/** A client route outside the known set (body §E.4). */
export const ROUTE_UNKNOWN = '__unknown__';

/** An action: there is no page, and `route` is `NOT NULL`. */
export const ROUTE_NONE = '__none__';

/** A two-letter uppercase ISO country, the only shape the header may carry. */
const COUNTRY_RE = /^[A-Z]{2}$/;

/** What every row needs before its own kind fills the rest. */
export interface RowContext {
  ctx: HttpRequestContext;
  rt: AnalyticsRuntime;
  identity: VisitorIdentity;
  ua: string | null;
  ts: Date;
  eventId: string;
  name: string;
  source: 'server' | 'client';
}

/**
 * Builds the envelope every analytics row shares, with every kind-specific
 * column at its neutral value.
 *
 * Starting from a fully-null row and letting each caller fill only what its
 * kind owns keeps a column from leaking across kinds — an action can not
 * accidentally carry the path of the page it fired on, because the action
 * builder never writes `path`.
 *
 * @param input - The request, the runtime, the visitor, and the event identity.
 * @returns A row with the envelope, the visitor, and the classified client filled.
 */
export function baseRow(input: RowContext): EventRow {
  const { browser, browserMajor, os, deviceType } = classifyUserAgent(input.ua);
  return {
    eventId: input.eventId,
    ts: input.ts,
    day: utcDay(input.ts),
    name: input.name,
    source: input.source,
    app: input.rt.app,
    env: input.rt.env,
    appVersion: input.rt.version,
    visitorId: input.identity.visitorId,
    visitorKind: input.identity.visitorKind,
    sessionId: null,
    seq: null,
    userId: userIdOf(input.ctx),
    route: ROUTE_NONE,
    path: null,
    referrer: null,
    referrerType: 'direct',
    utmSource: null,
    utmMedium: null,
    utmCampaign: null,
    utmContent: null,
    utmTerm: null,
    statusCode: null,
    renderMs: null,
    engagementMs: 0,
    actionName: null,
    outcome: null,
    props: {},
    browser,
    browserMajor,
    os,
    deviceType,
    language: primaryLanguage(input.ctx),
    viewportClass: null,
    country: countryOf(input.ctx, input.rt),
  };
}

/**
 * The authenticated subject, or null.
 *
 * Telling the *browser* about it is never done — the bootstrap has no field
 * for it.
 *
 * @param ctx - The request context.
 * @returns `ctx.user.sub`, or null for an anonymous request.
 */
export function userIdOf(ctx: HttpRequestContext): string | null {
  const sub = ctx.user?.sub;
  return typeof sub === 'string' && sub.length > 0 ? sub : null;
}

/**
 * The visitor's primary language tag, read from `Accept-Language`.
 *
 * Only the first tag survives, and only when it matches the protocol's
 * language rule: the full header is a fingerprinting-grade string and is never
 * stored.
 *
 * @param ctx - The request context.
 * @returns A validated language tag, or null.
 */
export function primaryLanguage(ctx: HttpRequestContext): string | null {
  const header = ctx.headers.get('accept-language');
  if (!header) {
    return null;
  }
  const first = (header.split(',')[0] ?? '').split(';')[0]?.trim() ?? '';
  return LANGUAGE_RE.test(first) ? first : null;
}

/**
 * The country, from the trusted edge header only (body §C).
 *
 * There is no GeoIP lookup by design: the address that would feed one is never
 * kept long enough to look anything up. An operator who has an edge that
 * already resolved the country names the header; anyone else stores null.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @returns A two-letter uppercase country, or null.
 */
export function countryOf(ctx: HttpRequestContext, rt: AnalyticsRuntime): string | null {
  const header = rt.config.countryHeader;
  if (!header) {
    return null;
  }
  const value = ctx.headers.get(header)?.toUpperCase().slice(0, 2);
  return value && COUNTRY_RE.test(value) ? value : null;
}
