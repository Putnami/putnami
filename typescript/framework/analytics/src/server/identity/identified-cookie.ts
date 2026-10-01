import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import type { HttpRequestContext } from '@putnami/application';
import { useConfig, useLogger } from '@putnami/runtime';
import { AnalyticsConfig, type AnalyticsConfigValues } from '../analytics.config';

/** Domain separator of the cookie signature, versioned like the day key. */
const SIGNATURE_LABEL = 'cookie/v1/';

/** The number of base64url characters kept from the signature digest. */
export const SIGNATURE_LENGTH = 22;

/** Seconds in a day, the unit `cookieMaxAgeDays` is expressed in. */
const SECONDS_PER_DAY = 86_400;

/**
 * The context slot the identified cookie waits in.
 *
 * `cookies()` from `@putnami/application` fixes `Max-Age` to the session TTL
 * and takes no options, so the analytics cookie is built here and attached by
 * the ingest route, which owns the response.
 */
export const ANALYTICS_SET_COOKIE_SLOT = '__putnamiAnalyticsSetCookie';

/** The runtime members the identity path needs; the plugin owns the rest. */
export interface AnalyticsRuntime {
  /** The resolved analytics configuration. */
  config: AnalyticsConfigValues;
  /** The server-side key, from `resolveSecret`. */
  secret: string;
  /** The application's consent gate; absent means nobody consented. */
  consent?: (ctx: HttpRequestContext) => boolean | Promise<boolean>;
}

/** A resolved identified visitor, and the cookie to set when it is new. */
export interface CookieDecision {
  /** The persistent first-party visitor id. */
  visitorId: string;
  /** The `Set-Cookie` value to emit, present only when the cookie was minted. */
  setCookie?: string;
}

let consentFailureLogged = false;

/**
 * Mints a fresh identified visitor id.
 *
 * @returns 16 random bytes, base64url encoded.
 */
export function mintId(): string {
  return randomBytes(16).toString('base64url');
}

/**
 * Signs an identified visitor id with the server secret.
 *
 * @param secret - The server-side key.
 * @param id - The visitor id to sign.
 * @returns A 22-character base64url signature.
 */
export function sign(secret: string, id: string): string {
  return createHmac('sha256', secret).update(`${SIGNATURE_LABEL}${id}`).digest('base64url').slice(0, SIGNATURE_LENGTH);
}

/**
 * Reads and verifies the analytics cookie out of a `Cookie` header.
 *
 * A forged or truncated signature is treated as no cookie at all, so the
 * request falls back to the cookieless identity instead of adopting an id a
 * visitor chose.
 *
 * @param header - The raw `Cookie` header, or null.
 * @param name - The configured cookie name.
 * @param secret - The server-side key.
 * @returns The verified visitor id, or undefined.
 */
export function parseCookie(header: string | null, name: string, secret: string): string | undefined {
  if (!header) {
    return undefined;
  }
  for (const part of header.split(';')) {
    const entry = part.trim();
    const separator = entry.indexOf('=');
    if (separator === -1 || entry.slice(0, separator) !== name) {
      continue;
    }
    return verifiedId(entry.slice(separator + 1), secret);
  }
  return undefined;
}

function verifiedId(value: string, secret: string): string | undefined {
  const dot = value.indexOf('.');
  if (dot <= 0) {
    return undefined;
  }
  const id = value.slice(0, dot);
  const expected = Buffer.from(sign(secret, id));
  const actual = Buffer.from(value.slice(dot + 1));
  // timingSafeEqual throws on unequal lengths, so the length is compared first
  // and a wrong-length signature is rejected before the constant-time compare.
  if (expected.length !== actual.length || !timingSafeEqual(expected, actual)) {
    return undefined;
  }
  return id;
}

/**
 * Renders a `Set-Cookie` value for the identified visitor.
 *
 * @param name - The cookie name.
 * @param value - The `<id>.<signature>` payload.
 * @param maxAgeSeconds - The lifetime; 0 deletes the cookie.
 * @param secure - Whether the request arrived over TLS.
 * @returns The header value the caller pushes onto the response.
 */
export function serializeCookie(name: string, value: string, maxAgeSeconds: number, secure: boolean): string {
  const attributes = `${name}=${value}; Path=/; Max-Age=${maxAgeSeconds}; HttpOnly; SameSite=Lax`;
  return secure ? `${attributes}; Secure` : attributes;
}

/**
 * Reports whether the visitor asked not to be tracked.
 *
 * @param ctx - The request context.
 * @param config - The resolved analytics configuration.
 * @returns True when `Sec-GPC` or `DNT` forbids the persistent cookie.
 */
export function optOutSignal(ctx: HttpRequestContext, config: AnalyticsConfigValues): boolean {
  return (
    (config.respectGpc && ctx.headers.get('Sec-GPC') === '1') || (config.respectDnt && ctx.headers.get('DNT') === '1')
  );
}

/**
 * Resolves the identified visitor, or nothing when the request must stay
 * cookieless (body §D.2).
 *
 * Fails closed at every gate: a mode other than `identified`, an opt-out
 * signal, a missing consent callback, a consent callback that returns false,
 * and a consent callback that throws all return undefined, and the caller
 * falls back to the daily hash. The cookie is minted only when consent was
 * given and no valid cookie exists yet.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime: config, secret, consent gate.
 * @returns The visitor id and, when freshly minted, the cookie to set.
 */
export async function resolveIdentifiedVisitor(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
): Promise<CookieDecision | undefined> {
  if (rt.config.mode !== 'identified' || optOutSignal(ctx, rt.config)) {
    return undefined;
  }
  if (!(await consented(ctx, rt))) {
    return undefined;
  }
  const existing = parseCookie(ctx.headers.get('Cookie'), rt.config.cookieName, rt.secret);
  if (existing !== undefined) {
    return { visitorId: existing };
  }
  const id = mintId();
  const value = `${id}.${sign(rt.secret, id)}`;
  const maxAge = rt.config.cookieMaxAgeDays * SECONDS_PER_DAY;
  return { visitorId: id, setCookie: serializeCookie(rt.config.cookieName, value, maxAge, ctx.secured()) };
}

async function consented(ctx: HttpRequestContext, rt: AnalyticsRuntime): Promise<boolean> {
  if (!rt.consent) {
    return false;
  }
  try {
    return (await rt.consent(ctx)) === true;
  } catch (error) {
    if (!consentFailureLogged) {
      consentFailureLogged = true;
      const message = error instanceof Error ? error.message : String(error);
      useLogger('@putnami/analytics').warn(`consent callback failed, treating the visitor as cookieless: ${message}`);
    }
    return false;
  }
}

/**
 * Queues a `Set-Cookie` value on the request context.
 *
 * @param ctx - The request context.
 * @param value - The header value to emit.
 */
export function pushPendingCookie(ctx: HttpRequestContext, value: string): void {
  const slot = ctx as unknown as Record<string, unknown>;
  const pending = slot[ANALYTICS_SET_COOKIE_SLOT];
  slot[ANALYTICS_SET_COOKIE_SLOT] = Array.isArray(pending) ? [...(pending as string[]), value] : [value];
}

/**
 * Drains the queued `Set-Cookie` values.
 *
 * @param ctx - The request context.
 * @returns The values queued for this request, in order.
 */
export function takePendingCookies(ctx: HttpRequestContext): string[] {
  const slot = ctx as unknown as Record<string, unknown>;
  const pending = slot[ANALYTICS_SET_COOKIE_SLOT];
  slot[ANALYTICS_SET_COOKIE_SLOT] = undefined;
  return Array.isArray(pending) ? (pending as string[]) : [];
}

/**
 * Forgets the identified visitor: queues a deletion of the analytics cookie.
 *
 * The next request is measured cookielessly, which needs no erasure request —
 * the daily hash expires by rotation.
 *
 * @param ctx - The request context.
 */
export function forget(ctx: HttpRequestContext): void {
  const { cookieName } = useConfig(AnalyticsConfig);
  pushPendingCookie(ctx, serializeCookie(cookieName, '', 0, ctx.secured()));
}

/**
 * Re-arms the once-only consent-failure warning.
 *
 * The warning is emitted once per process so a throwing callback cannot flood
 * the log on every request; tests reset it to observe it again.
 */
export function resetConsentWarning(): void {
  consentFailureLogged = false;
}
