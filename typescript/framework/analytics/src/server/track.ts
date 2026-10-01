import type { HttpRequestContext } from '@putnami/application';
import { validateProps } from './declare';
import { baseRow, ROUTE_NONE } from './http/rows';
import { resolveVisitor } from './http/visitor';
import { type AnalyticsRuntime, optionalAnalyticsRuntime } from './runtime';
import { EVENT_ACTION } from './sanitize/vocabulary';
import { uuidv7 } from './uuidv7';

/**
 * Records one declared action from server code (body §E.6).
 *
 * The undeclared-name check throws **synchronously**, before the first await:
 * a name that is not declared is a developer mistake, not a runtime condition,
 * and a rejected promise nobody awaited would swallow it. The wire path makes
 * the opposite choice for the same rule — a browser sending an undeclared name
 * is told nothing at all — because only one of the two callers can fix it.
 *
 * @param ctx - The request context, for the visitor and the authenticated user.
 * @param name - A name previously passed to `declareEvents` or `analytics({ events })`.
 * @param props - Properties; only declared keys of the declared type survive.
 * Measuring is optional; the application is not. When analytics is absent or
 * switched off with `analytics.enabled: false` — a documented operator switch —
 * this records nothing and returns, rather than turning every endpoint that
 * measures into a 500. The client `track()` makes the same choice for the same
 * situation.
 *
 * @returns A promise that settles when the row is accepted into the write
 * queue, or immediately when there is nothing to record. It never waits for
 * the database: the row lands on a later flush, and is dropped under pressure
 * rather than delaying the endpoint that measured itself.
 * @throws Error when the name is not declared and analytics is running.
 */
export function track(
  ctx: HttpRequestContext,
  name: string,
  props: Record<string, string | number | boolean> = {},
): Promise<void> {
  const rt = optionalAnalyticsRuntime();
  if (!rt) {
    return Promise.resolve();
  }
  if (!rt.declared.has(name)) {
    throw new Error(`analytics: action "${name}" is not declared (declareEvents)`);
  }
  return writeAction(ctx, rt, name, props);
}

/** Builds the action row and hands it to the write queue. */
async function writeAction(
  ctx: HttpRequestContext,
  rt: AnalyticsRuntime,
  name: string,
  props: Record<string, string | number | boolean>,
): Promise<void> {
  const ua = ctx.headers.get('user-agent');
  const identity = await resolveVisitor(ctx, rt, ua);
  const row = baseRow({
    ctx,
    rt,
    identity,
    ua,
    ts: rt.now(),
    eventId: uuidv7(),
    name: EVENT_ACTION,
    source: 'server',
  });
  row.actionName = name;
  row.props = validateProps(rt.declaredSchemas[name], props);
  row.route = ROUTE_NONE;
  // Accepted, not written. The flush belongs to the elected request, the
  // ticker, and `stop()`; a caller that must see the row now — a test, or an
  // operator before a maintenance stop — calls `flushAnalytics()`.
  rt.queue.enqueue([row]);
}
