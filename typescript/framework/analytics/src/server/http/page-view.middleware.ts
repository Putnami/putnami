import type { HttpMiddleware, HttpRequestContext, HttpResponse } from '@putnami/application';
import {
  ACTION_OUTCOME_CONTEXT_KEY,
  type ActionOutcomeSlot,
  clientBootstrapEmitted,
  contextSlots,
  mergeClientBootstrap,
  pushClientScript,
} from '@putnami/web';
import { isBot } from '../enrich/bots';
import { classifyReferrer, extractUtm, normalizePath } from '../enrich/referrer';
import type { AnalyticsRuntime } from '../runtime';
import { EVENT_FORM_SUBMIT, EVENT_PAGE_VIEW } from '../sanitize/vocabulary';
import type { EventRow } from '../sink/fold';
import { awaitFlush } from '../sink/queue';
import { uuidv7 } from '../uuidv7';
import { type AnalyticsBootstrap, buildBootstrap } from './bootstrap';
import { injectTrackerTag } from './inject';
import { baseRow, ROUTE_UNMATCHED } from './rows';
import { attachPendingCookies, resolveVisitor, type VisitorIdentity } from './visitor';

/** The server-minted page view, opened before the handler renders. */
interface PageViewStart {
  eventId: string;
  startedAt: number;
  /** What the browser is told about this view, whichever way it reaches it. */
  bootstrap: AnalyticsBootstrap;
}

/**
 * The global middleware that records server-side page views and form submits,
 * and hands the browser its bootstrap (body §E.2).
 *
 * The event id is minted **before** the handler runs so the same id can travel
 * two ways: into the HTML, where the tracker later re-sends it with engagement,
 * and into the database as the server's own record. That is the whole reason a
 * page view survives an ad blocker: the row exists whether or not the tracker
 * ever loaded.
 *
 * It is also where the write queue gets its CPU. Every request — a static page,
 * a JSON call, a request that produces no row at all — offers the queue a flush
 * **before** `next()`, and at most one request every `flushIntervalMs` is
 * elected to start one. That request lets the flush overlap its own render and
 * then waits for it, for at most `flushWaitMs`. Nothing else here awaits the
 * database: a request that is not elected pays one timestamp comparison.
 *
 * @param rt - The analytics runtime.
 * @returns The middleware to register with `httpPlugin.use`.
 */
export function pageViewMiddleware(rt: AnalyticsRuntime): HttpMiddleware {
  return async (ctx, next) => {
    // Before the handler: on request-based CPU this is the window that is
    // guaranteed to run, so a flush started here overlaps the render instead
    // of being throttled between requests.
    const flushing = rt.queue.kick();
    const ua = ctx.headers.get('user-agent');
    const bot = isBot(ua);
    const pv = bot || ctx.method !== 'GET' ? undefined : openPageView(ctx, rt);

    const res = await next();
    if (!res || bot) {
      await awaitFlush(flushing, rt.config.flushWaitMs);
      return res;
    }

    await recordRequest(ctx, rt, res, pv, ua);
    const answered = withTracker(ctx, rt, res, pv);
    // The elected request, and only it, pays: the flush it started minus the
    // render it already spent, capped. A flush that outruns the cap keeps
    // running and the response goes out anyway.
    await awaitFlush(flushing, rt.config.flushWaitMs);
    return attachPendingCookies(ctx, answered);
  };
}

/**
 * Mints the page-view id and publishes it to the browser, before the handler
 * renders anything.
 *
 * `ctx.route` is the matched pattern, populated before the middleware chain
 * runs. Falling back to the raw path would let an attacker turn a 404 sweep
 * into an unbounded counter series, so an unmatched request folds under one
 * sentinel instead.
 */
function openPageView(ctx: HttpRequestContext, rt: AnalyticsRuntime): PageViewStart {
  const bootstrap = buildBootstrap(rt, uuidv7(), ctx.route ?? ROUTE_UNMATCHED);
  const pv: PageViewStart = { eventId: bootstrap.pv, startedAt: rt.now().getTime(), bootstrap };
  mergeClientBootstrap(ctx, { analytics: bootstrap });
  if (rt.trackerUrl) {
    pushClientScript(ctx, rt.trackerUrl);
  }
  return pv;
}

/**
 * Gives a page the renderer never touched the tracker it would otherwise miss.
 *
 * A `.static()` route answers with bytes rendered at build time: the request
 * slots this middleware wrote before `next()` are read by nobody, so the
 * document reaches the browser with no bootstrap and no script tag. Every page
 * of a fully pre-rendered site is in that shape, and without this the tracker
 * would never install on one — no session, no engagement, no declared action.
 *
 * The injection carries *this request's* page-view id, so the browser enriches
 * the row the server just recorded rather than minting a competing one. A
 * streamed SSR response is skipped on both counts: its renderer already
 * emitted the bootstrap, and its body is a stream this could not rewrite
 * without buffering the page it exists not to delay.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @param res - The response the handler produced.
 * @param pv - The page view opened before the handler ran, when there is one.
 * @returns The response to answer with — the same one whenever nothing was injected.
 */
function withTracker(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  res: HttpResponse,
  pv: PageViewStart | undefined,
): HttpResponse {
  if (!pv || !rt.trackerUrl || clientBootstrapEmitted(ctx) || !isRenderedPage(res)) {
    return res;
  }
  const body = res.getBodyInit();
  if (typeof body !== 'string') {
    return res;
  }
  const injected = injectTrackerTag(body, pv.bootstrap, rt.trackerUrl);
  return injected === undefined ? res : res.withBody(injected);
}

/** Resolves the visitor once and queues whatever this request produced. */
async function recordRequest(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  res: HttpResponse,
  pv: PageViewStart | undefined,
  ua: string | null,
): Promise<void> {
  const identity = await resolveVisitor(ctx, rt, ua);
  const pending: EventRow[] = [];
  if (pv && rt.config.serverPageViews && isRenderedPage(res)) {
    pending.push(serverPageViewRow(ctx, rt, pv, res, identity, ua));
  }
  const outcome = contextSlots(ctx)[ACTION_OUTCOME_CONTEXT_KEY] as ActionOutcomeSlot | undefined;
  if (outcome) {
    pending.push(formSubmitRow(ctx, rt, outcome, res, identity, ua));
  }
  if (pending.length > 0) {
    // Accepted, not written: the row lands on a later flush, or is dropped
    // under pressure. A response that waited for Postgres would be the
    // measurement degrading the thing it measures. No kick here on purpose —
    // a flush started after the response has no CPU window to run in, and it
    // would hold the one in-flight slot the next elected request needs.
    rt.queue.enqueue(pending);
  }
}

/**
 * Reports whether a response is a page a human just read.
 *
 * The header lookup is case-insensitive because the SSR renderer writes
 * `content-type` in lowercase while other producers write `Content-Type`, and
 * `HttpResponse.getHeader` compares names exactly.
 *
 * @param res - The response the handler produced.
 * @returns True for a successful HTML document.
 */
export function isRenderedPage(res: HttpResponse): boolean {
  if ((res.status ?? 200) >= 400) {
    return false;
  }
  const contentType = res.getHeaderEntries().find(([name]) => name.toLowerCase() === 'content-type')?.[1] ?? '';
  return contentType.toLowerCase().startsWith('text/html');
}

/**
 * Builds the server-side page view (body §E.2).
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @param pv - The event id and start instant minted before the handler ran.
 * @param res - The rendered response.
 * @param identity - The resolved visitor.
 * @param ua - The `User-Agent` header.
 * @returns One `page_view` row.
 */
export function serverPageViewRow(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  pv: PageViewStart,
  res: HttpResponse,
  identity: VisitorIdentity,
  ua: string | null,
): EventRow {
  const now = rt.now();
  const referrer = classifyReferrer(ctx.headers.get('referer'), ctx.host());
  const utm = extractUtm(ctx.queryParams());
  return {
    ...baseRow({ ctx, rt, identity, ua, ts: now, eventId: pv.eventId, name: EVENT_PAGE_VIEW, source: 'server' }),
    route: ctx.route ?? ROUTE_UNMATCHED,
    path: normalizePath(ctx.path()),
    referrer: referrer.referrer,
    referrerType: referrer.referrerType,
    utmSource: utm.source ?? null,
    utmMedium: utm.medium ?? null,
    utmCampaign: utm.campaign ?? null,
    utmContent: utm.content ?? null,
    utmTerm: utm.term ?? null,
    statusCode: res.status ?? 200,
    renderMs: Math.max(0, now.getTime() - pv.startedAt),
  };
}

/**
 * Builds the server-side form submission (body §E.2).
 *
 * Everything page-specific stays null: a form submit is an outcome on a route,
 * not a view, and giving it a path would double-count the page it posted from.
 *
 * @param ctx - The request context.
 * @param rt - The analytics runtime.
 * @param outcome - What the action handler published on the request slot.
 * @param res - The response the action produced.
 * @param identity - The resolved visitor.
 * @param ua - The `User-Agent` header.
 * @returns One `form_submit` row.
 */
export function formSubmitRow(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  outcome: ActionOutcomeSlot,
  res: HttpResponse,
  identity: VisitorIdentity,
  ua: string | null,
): EventRow {
  return {
    ...baseRow({
      ctx,
      rt,
      identity,
      ua,
      ts: rt.now(),
      eventId: uuidv7(),
      name: EVENT_FORM_SUBMIT,
      source: 'server',
    }),
    route: outcome.route ?? ROUTE_UNMATCHED,
    outcome: outcome.outcome,
    statusCode: res.status ?? 200,
  };
}
