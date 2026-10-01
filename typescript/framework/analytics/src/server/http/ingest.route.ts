import {
  type HttpPlugin,
  type HttpRequestContext,
  HttpResponse,
  incCounter,
  RateLimitMiddleware,
} from '@putnami/application';
import { validateProps } from '../declare';
import { isBot } from '../enrich/bots';
import { classifyReferrer } from '../enrich/referrer';
import type { AnalyticsRuntime } from '../runtime';
import type { SanitizedEvent } from '../sanitize/sanitizer';
import { sanitizeBatch } from '../sanitize/sanitizer';
import { EVENT_ACTION, MAX_BODY_BYTES } from '../sanitize/vocabulary';
import type { EventRow } from '../sink/fold';
import { baseRow, ROUTE_NONE, ROUTE_UNKNOWN } from './rows';
import { attachPendingCookies, resolveVisitor, type VisitorIdentity } from './visitor';

/** Where the browser POSTs its batches, before any module base path. */
export const INGEST_PATH = '/_putnami/analytics/events';

/** The rate-limit window; `rateLimitPerMinute` is expressed against it. */
export const RATE_LIMIT_WINDOW_MS = 60_000;

/** How far behind the receive instant a skewed client clock may place an event. */
export const MAX_CLOCK_SKEW_MS = 24 * 60 * 60 * 1000;

/** The content types a beacon may use: `fetch` sends JSON, `sendBeacon` text. */
const ACCEPTED_TYPES = ['application/json', 'text/plain'];

/** The opaque acceptance: 202, empty body, always the same bytes. */
const accepted = (): HttpResponse => new HttpResponse(undefined, { status: 202 });

/**
 * Answers without reading the body, and tells the sender to stop uploading.
 *
 * Buffering a body already known to be too large — or of a type nothing here
 * can parse — would hand an attacker the memory the size cap exists to protect.
 * Cancelling the stream releases the connection instead of leaving a half-sent
 * upload holding it open.
 */
function refuse(ctx: HttpRequestContext, status: number): HttpResponse {
  const body = ctx.req.body;
  if (body) {
    body.cancel().catch(() => undefined);
  }
  return new HttpResponse(undefined, { status });
}

/** Everything `toRow` needs besides the sanitized event. */
interface RowInput {
  ctx: HttpRequestContext;
  rt: AnalyticsRuntime;
  identity: VisitorIdentity;
  ua: string | null;
  receivedAt: Date;
  sentAt: Date | null;
}

/**
 * Registers the CSRF-exempt, rate-limited ingest route (body §E.1).
 *
 * `csrfExempt` is not a relaxation of the app's posture but its precondition:
 * `react({ csrf: true })` is the default, a `sendBeacon` cannot carry a token,
 * and the route is unauthenticated by design. What bounds it instead is the
 * per-peer rate limit, the 64 KiB body cap, the 50-event batch cap, and the
 * closed vocabulary the sanitizer enforces — none of which a token would add.
 *
 * The limiter is handed the *same* key generator the visitor hash keys on, so
 * one request has one notion of who its peer is.
 *
 * @param httpPlugin - The HTTP plugin the route is registered on.
 * @param rt - The analytics runtime.
 */
export function registerIngestRoute(httpPlugin: HttpPlugin, rt: AnalyticsRuntime): void {
  const limiter = RateLimitMiddleware({
    max: rt.config.rateLimitPerMinute,
    windowMs: RATE_LIMIT_WINDOW_MS,
    keyGenerator: rt.keyGenerator,
    // No `RateLimit-*` headers: they would tell a sender how close it is to
    // the bucket, which is one more oracle than an opaque receiver may offer.
    headers: false,
  });
  httpPlugin.post(rt.endpoint, (ctx) => limiter(ctx, () => ingest(ctx, rt)), { csrfExempt: true });
}

/**
 * Handles one batch (body §A.3).
 *
 * The response is an oracle for nothing: `202` is returned byte-identically
 * whether every event was stored, some were, or all were dropped. A sender
 * that could tell acceptance from a silent drop could enumerate the declared
 * vocabulary and the known-route set by observation. Drops are counted in
 * metrics instead, where only the operator reads them.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @returns 202 for anything semantic, 400 for an unreadable body, 413 when oversized.
 */
export async function ingest(ctx: HttpRequestContext, rt: AnalyticsRuntime): Promise<HttpResponse> {
  const type = (ctx.headers.get('content-type') ?? '').toLowerCase();
  if (!ACCEPTED_TYPES.some((prefix) => type.startsWith(prefix))) {
    return refuse(ctx, 400);
  }
  if (Number(ctx.headers.get('content-length') ?? '0') > MAX_BODY_BYTES) {
    return refuse(ctx, 413);
  }
  const text = await ctx.req.text();
  // The declared length is a claim; the read length is the fact.
  if (Buffer.byteLength(text) > MAX_BODY_BYTES) {
    return new HttpResponse(undefined, { status: 413 });
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return new HttpResponse(undefined, { status: 400 });
  }

  const ua = ctx.headers.get('user-agent');
  if (isBot(ua)) {
    incCounter('analytics.ingest.dropped.bot');
    return accepted();
  }

  const result = sanitizeBatch(parsed, rt.declared);
  for (const drop of result.dropped) {
    incCounter(`analytics.ingest.dropped.${drop.reason}`);
  }
  if (result.events.length === 0) {
    return accepted();
  }

  const identity = await resolveVisitor(ctx, rt, ua);
  const receivedAt = rt.now();
  const rows = result.events.map((event) => toRow(event, { ctx, rt, identity, ua, receivedAt, sentAt: result.sentAt }));
  // Parsed, sanitized, and queued — all cheap CPU. The sink is never awaited
  // here: a beacon that held a Postgres transaction open would let a slow
  // database turn a burst of `sendBeacon` calls into a connection pile-up, and
  // 202 already means "accepted", never "stored". No flush is started here
  // either: the global middleware already offered this very request a flush
  // before the route ran, and a second start would only take the one in-flight
  // slot without anyone waiting for it.
  rt.queue.enqueue(rows);
  return attachPendingCookies(ctx, accepted());
}

/**
 * Turns one sanitized wire event into a database row (body §D.6–§D.8, §E.4).
 *
 * @param event - The event the sanitizer accepted.
 * @param input - The request, the runtime, the visitor, and the batch timing.
 * @returns The row to write.
 */
export function toRow(event: SanitizedEvent, input: RowInput): EventRow {
  const ts = canonicalTs(event.clientTs, input.receivedAt, input.sentAt);
  const row = baseRow({
    ctx: input.ctx,
    rt: input.rt,
    identity: input.identity,
    ua: input.ua,
    ts,
    eventId: event.eventId,
    name: event.kind,
    source: 'client',
  });
  row.sessionId = event.sessionId;
  row.seq = event.seq;
  row.engagementMs = event.engagementMs ?? 0;
  row.viewportClass = event.viewportClass as EventRow['viewportClass'];
  // The client's own language beats the header: it is what the page rendered in.
  row.language = event.language ?? row.language;

  if (event.kind === EVENT_ACTION) {
    row.actionName = event.action;
    row.props = validateProps(input.rt.declaredSchemas[event.action], event.props);
    // The wire `action` event carries no page, and `route` is NOT NULL.
    row.route = ROUTE_NONE;
    return row;
  }

  // A route the application does not serve is not a route: storing the
  // client's string verbatim would let a browser mint counter keys at will.
  row.route = event.route && input.rt.knownRoutes.has(event.route) ? event.route : ROUTE_UNKNOWN;
  row.path = event.path;
  const referrer = classifyReferrer(event.referrer, input.ctx.host());
  row.referrer = referrer.referrer;
  row.referrerType = referrer.referrerType;
  row.utmSource = event.utm.source ?? null;
  row.utmMedium = event.utm.medium ?? null;
  row.utmCampaign = event.utm.campaign ?? null;
  row.utmContent = event.utm.content ?? null;
  row.utmTerm = event.utm.term ?? null;
  return row;
}

/**
 * The canonical event time (body §D.8).
 *
 * A browser clock can be anything, so the client instant is re-based on the
 * batch's own send time and then clamped into the last 24 hours: a skewed or
 * hostile clock can move an event inside that window, never outside it, so no
 * client can write into a day that has already been aggregated.
 *
 * @param clientTs - The instant the client stamped on the event.
 * @param receivedAt - When the server read the batch.
 * @param sentAt - When the client says it sent the batch.
 * @returns The instant to store.
 */
export function canonicalTs(clientTs: Date, receivedAt: Date, sentAt: Date | null): Date {
  if (!sentAt) {
    return receivedAt;
  }
  const rebased = clientTs.getTime() + (receivedAt.getTime() - sentAt.getTime());
  const floor = receivedAt.getTime() - MAX_CLOCK_SKEW_MS;
  return new Date(Math.min(receivedAt.getTime(), Math.max(floor, rebased)));
}
