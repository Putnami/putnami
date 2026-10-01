import type { NavigationDetail } from '@putnami/web';
import {
  bounded,
  MAX_PATH_LEN,
  MAX_PROP_KEYS,
  MAX_PROP_STRING_LEN,
  MAX_REFERRER_LEN,
  MAX_ROUTE_LEN,
  MAX_UTM_LEN,
  PROP_KEY_RE,
  utf8Bytes,
} from './bounds';
import { createEngagement, type Engagement } from './engagement';
import { createQueue, type Queue, queueKey } from './queue';
import { installDataTrack } from './react/data-track';
import { createSession, type Session } from './session';
import { send } from './transport';
import { uuidv7 } from './uuidv7';
import { clientViewportClass } from './viewport';
import type { ClientBootstrap, TrackFn, WireEvent, WirePage, WireUtm } from './wire';

/**
 * Name of the DOM event `@putnami/web` dispatches on a client navigation.
 *
 * The literal is repeated rather than imported: importing the constant from
 * `@putnami/web` would put a runtime edge from the tracker to the whole web
 * browser graph, and the tracker has a 4 KiB budget. Only the type crosses.
 */
export const NAVIGATION_EVENT = 'putnami:navigation';

/** The queue is flushed every five seconds when it is not empty. */
const FLUSH_INTERVAL_MS = 5000;
/** Twenty queued events flush immediately instead of waiting for the timer. */
const FLUSH_THRESHOLD = 20;
/** The wire ceiling: a batch never carries more than fifty events. */
const MAX_BATCH = 50;
/** Backoff never grows past a minute. */
const MAX_BACKOFF_MS = 60_000;

/** BCP 47 primary tag with an optional subtag (protocol `LanguageRe`). */
const LANGUAGE_RE = /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$/;
/** The five campaign parameters, in the wire's key order. */
const UTM_KEYS = ['source', 'medium', 'campaign', 'content', 'term'] as const;

/** What a page view carries beyond the shared envelope. */
interface PageContext {
  referrer?: string;
  utm?: WireUtm;
}

/** The page view the tracker is currently accumulating engagement for. */
interface View {
  eventId: string;
  route: string;
  path: string;
  /**
   * The enrichment that must ride on *every* send of this view.
   *
   * It is kept on the view rather than passed per call because the queue
   * replaces an entry with a matching `eventId`: an engagement re-send that
   * carried no referrer would overwrite the queued initial view and the
   * attribution would never leave the browser. A visitor who clicks a link
   * within five seconds of landing is the common case, not the corner one.
   */
  context: PageContext;
}

/** Everything one installed tracker owns. */
interface TrackerState {
  boot: ClientBootstrap;
  queue: Queue;
  session: Session;
  engagement: Engagement;
  declared: Set<string>;
  currentView: View;
  disposers: (() => void)[];
  flushTimer?: ReturnType<typeof setTimeout>;
  attempt: number;
  backoffUntil: number;
}

let state: TrackerState | undefined;

/**
 * Normalizes `navigator.language` to a tag the wire accepts.
 *
 * `zh-Hans-CN` has three subtags and the protocol allows one, so the tag is
 * shortened rather than dropped: the language of a visitor is a useful
 * dimension and an unparsable one would be silently lost.
 */
function wireLanguage(tag: string | undefined): string | undefined {
  const parts = (tag ?? '').split('-');
  for (const candidate of [parts.slice(0, 2).join('-'), parts[0] ?? '']) {
    if (LANGUAGE_RE.test(candidate)) {
      return candidate;
    }
  }
  return undefined;
}

/**
 * Normalizes a referrer to the wire shape, dropping what the server rejects.
 *
 * Query and fragment are stripped **here**, before the value leaves the
 * browser: the server stores `origin + pathname` anyway, and a search string
 * can carry the very identifiers this package exists not to collect.
 * `//host` and `/\host` are refused because a browser resolves both against
 * the current scheme, so accepting them would file another origin as internal.
 */
export function wireReferrer(value: string | undefined): string | undefined {
  const trimmed = (value ?? '').split('?')[0].split('#')[0];
  if (!trimmed || utf8Bytes(trimmed) > MAX_REFERRER_LEN) {
    return undefined;
  }
  if (trimmed.startsWith('/')) {
    return trimmed[1] === '/' || trimmed[1] === '\\' ? undefined : trimmed;
  }
  return /^https?:\/\/[^/?#]/.test(trimmed) ? trimmed : undefined;
}

/**
 * Reads the five campaign parameters out of a location search string.
 *
 * Nothing else from the query is read, ever. Empty values are dropped: the
 * protocol wants 1 to 128 bytes, and `?utm_source=` is not an attribution.
 */
export function utmFromSearch(search: string): WireUtm {
  const utm: WireUtm = {};
  const params = new URLSearchParams(search);
  for (const key of UTM_KEYS) {
    const value = (params.get(`utm_${key}`) ?? '').trim();
    if (value) {
      utm[key] = bounded(value, MAX_UTM_LEN);
    }
  }
  return utm;
}

/**
 * Keeps a route only when it is a real pattern.
 *
 * The server bootstraps `__unmatched__` when nothing matched and the router
 * reports `__unknown__` when it cannot name the leaf. Neither starts with `/`,
 * so both fail the protocol's route regex — sending one would drop the whole
 * page view instead of the route alone.
 */
function wireRoute(route: string): string | undefined {
  return route.startsWith('/') ? bounded(route, MAX_ROUTE_LEN) : undefined;
}

/** Drops properties the protocol would reject, and bounds the ones it keeps. */
function wireProps(props: Record<string, string | number | boolean>): Record<string, string | number | boolean> {
  const out: Record<string, string | number | boolean> = {};
  for (const [key, value] of Object.entries(props)) {
    if (!PROP_KEY_RE.test(key) || Object.keys(out).length >= MAX_PROP_KEYS) {
      continue;
    }
    if (typeof value === 'string') {
      out[key] = bounded(value, MAX_PROP_STRING_LEN);
    } else if (typeof value === 'boolean' || Number.isFinite(value)) {
      out[key] = value;
    }
  }
  return out;
}

/** The envelope every event shares (body §A.2), stamped with the session. */
function baseEvent(eventId: string, name: 'page_view' | 'action'): WireEvent {
  const current = state as TrackerState;
  const { sessionId, seq } = current.session.next();
  const event: WireEvent = { eventId, name, clientTs: new Date().toISOString(), seq, sessionId };
  const width = window.innerWidth;
  if (typeof width === 'number') {
    event.viewportClass = clientViewportClass(width);
  }
  const language = wireLanguage(navigator.language);
  if (language) {
    event.language = language;
  }
  return event;
}

/** Builds one `page_view` for the given view, enrichment included. */
function pageViewEvent(view: View, engagementMs?: number): WireEvent {
  const event = baseEvent(view.eventId, 'page_view');
  const page: WirePage = { path: bounded(view.path, MAX_PATH_LEN) };
  const route = wireRoute(view.route);
  if (route) {
    page.route = route;
  }
  const { referrer, utm } = view.context;
  if (referrer) {
    page.referrer = referrer;
  }
  if (utm && Object.keys(utm).length > 0) {
    page.utm = utm;
  }
  event.page = page;
  if (engagementMs !== undefined) {
    event.engagementMs = engagementMs;
  }
  return event;
}

/** Builds one `action` with a fresh id. */
function actionEvent(name: string, props: Record<string, string | number | boolean>): WireEvent {
  const event = baseEvent(uuidv7(), 'action');
  event.action = name;
  const wire = wireProps(props);
  if (Object.keys(wire).length > 0) {
    event.props = wire;
  }
  return event;
}

/**
 * Re-sends the current view with the engagement it has accumulated.
 *
 * The `eventId` is the one already stored, so this is an update, not a second
 * page view: the sink takes `GREATEST(stored, incoming)` on `engagement_ms`
 * and moves no counter. Minting a fresh id here would double every page.
 */
function emitEngagement(): void {
  const current = state;
  if (!current) {
    return;
  }
  enqueue(pageViewEvent(current.currentView, current.engagement.total()));
}

/** Handles one client-side route change. */
function onNavigate(detail: NavigationDetail | undefined): void {
  const current = state;
  if (!current || !detail) {
    return;
  }
  emitEngagement();
  current.engagement.reset();
  // No UTM: campaign parameters belong to the landing URL, and carrying them
  // forward would attribute every in-app click to the same campaign.
  current.currentView = {
    eventId: uuidv7(),
    route: detail.route,
    path: detail.pathname,
    context: { referrer: wireReferrer(detail.previous) },
  };
  enqueue(pageViewEvent(current.currentView));
}

/** Adds a listener and records how to remove it. */
function listen(target: EventTarget, type: string, handler: (event: Event) => void): void {
  target.addEventListener(type, handler);
  state?.disposers.push(() => target.removeEventListener(type, handler));
}

/** Reacts to the tab being hidden or shown. */
function onVisibilityChange(): void {
  const current = state;
  if (!current) {
    return;
  }
  if (document.visibilityState === 'hidden') {
    current.engagement.pause();
    // biome-ignore lint/complexity/noVoid: fire-and-forget; flush reschedules itself and never rejects
    void flush({ unload: true });
  } else {
    current.engagement.resume();
  }
}

/** Reacts to the page going away: last engagement, last beacon. */
function onPageHide(): void {
  const current = state;
  if (!current) {
    return;
  }
  current.engagement.pause();
  emitEngagement();
  // biome-ignore lint/complexity/noVoid: fire-and-forget; flush reschedules itself and never rejects
  void flush({ unload: true });
}

/** Queues an event, flushing early once the queue is deep enough. */
function enqueue(event: WireEvent): void {
  const current = state;
  if (!current) {
    return;
  }
  current.queue.push(event);
  if (current.queue.size() >= FLUSH_THRESHOLD) {
    // biome-ignore lint/complexity/noVoid: fire-and-forget; flush reschedules itself and never rejects
    void flush({ unload: false });
  }
}

/** Arms the five-second flush timer, replacing any pending one. */
function scheduleFlush(): void {
  const current = state;
  if (!current) {
    return;
  }
  clearTimeout(current.flushTimer);
  current.flushTimer = setTimeout(() => {
    // biome-ignore lint/complexity/noVoid: fire-and-forget; flush reschedules itself and never rejects
    void flush({ unload: false });
  }, FLUSH_INTERVAL_MS);
}

/**
 * Sends one batch and decides what happens to it.
 *
 * Exported so tests can drive the loop instead of waiting five real seconds.
 *
 * A backoff is skipped on unload: the page is closing, this is the last
 * chance, and the beacon costs the visitor nothing. Backoff is exponential
 * with ±20 % jitter so a server coming back up does not meet every open tab in
 * the same millisecond.
 *
 * @param opts - `unload` selects the beacon transport and ignores the backoff.
 */
export async function flush(opts: { unload: boolean }): Promise<void> {
  const current = state;
  if (!current) {
    return;
  }
  if (Date.now() < current.backoffUntil && !opts.unload) {
    scheduleFlush();
    return;
  }
  const batch = current.queue.take(MAX_BATCH);
  if (batch.length === 0) {
    scheduleFlush();
    return;
  }
  const outcome = await send(current.boot.endpoint, batch, opts);
  if (outcome === 'retry') {
    current.attempt += 1;
    const delay = Math.min(MAX_BACKOFF_MS, 1000 * 2 ** current.attempt);
    current.backoffUntil = Date.now() + delay * (0.8 + Math.random() * 0.4);
  } else if (outcome !== 'pending') {
    // Only a response-confirmed outcome acknowledges. 'drop' acknowledges too:
    // the server has decided it will never take this batch, so keeping it would
    // retry a rejection until the queue overflows. A pending beacon has no such
    // response and deliberately leaves the durable batch untouched.
    current.queue.ack(batch.map((event) => event.eventId));
    current.attempt = 0;
    current.backoffUntil = 0;
  }
  scheduleFlush();
}

/**
 * Records one declared action.
 *
 * A no-op when the tracker is not installed — on the server, on a static page,
 * and in a unit test — so a component may call it unconditionally.
 *
 * @param name - The action name, as declared server-side.
 * @param props - The declared properties.
 */
export function track(name: string, props: Record<string, string | number | boolean> = {}): void {
  const current = state;
  if (!current) {
    return;
  }
  if (!current.declared.has(name)) {
    // The server would drop it as `unknown_action` without telling anyone, so
    // the warning is the only place a developer can learn about the typo.
    // biome-ignore lint/suspicious/noConsole: the only feedback channel for an undeclared action
    console.warn('[putnami:analytics] undeclared action', name);
    return;
  }
  enqueue(actionEvent(name, props));
}

/**
 * Installs the tracker for the page the server just rendered.
 *
 * The first thing it does is re-send the **server's** page view under the
 * server's event id, enriched with what only the browser knows: the session,
 * the referrer, the campaign parameters, and the viewport class. The row
 * already exists, `ON CONFLICT (event_id)` merges the enrichment into it, and
 * the page is counted exactly once whether the tracker ever loads or not.
 *
 * Install is idempotent: a second call returns immediately, so a page that
 * somehow carries the script twice does not double-count itself.
 *
 * @param boot - `window.__putnamiBootstrap.analytics`, written by the renderer.
 */
export function installTracker(boot: ClientBootstrap): void {
  if (state) {
    return;
  }
  const target = window;
  state = {
    boot,
    queue: createQueue(queueKey(boot)),
    session: createSession(),
    engagement: createEngagement(),
    declared: new Set(boot.declared),
    currentView: {
      eventId: boot.pv,
      route: boot.route,
      path: target.location.pathname,
      context: { referrer: wireReferrer(document.referrer), utm: utmFromSearch(target.location.search) },
    },
    disposers: [],
    attempt: 0,
    backoffUntil: 0,
  };

  enqueue(pageViewEvent(state.currentView));

  // `onNavigation` does not fire for the initial load — the bootstrap above is
  // the initial view — so the tracker only listens for what comes after it.
  listen(target, NAVIGATION_EVENT, (event) => onNavigate((event as CustomEvent<NavigationDetail>).detail));
  listen(document, 'visibilitychange', onVisibilityChange);
  listen(target, 'pagehide', onPageHide);
  state.disposers.push(installDataTrack(track));

  scheduleFlush();
  (target as unknown as { __putnamiAnalytics: { track: TrackFn } }).__putnamiAnalytics = { track };
}

/**
 * Removes every listener and forgets the installed state.
 *
 * The queue stays in `localStorage`: an uninstall is not an acknowledgement,
 * and the next page picks up whatever was still waiting.
 */
export function uninstallTracker(): void {
  const current = state;
  if (!current) {
    return;
  }
  clearTimeout(current.flushTimer);
  for (const dispose of current.disposers) {
    dispose();
  }
  const target = (globalThis as { window?: { __putnamiAnalytics?: unknown } }).window;
  if (target) {
    target.__putnamiAnalytics = undefined;
  }
  state = undefined;
}
