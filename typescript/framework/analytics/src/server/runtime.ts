import type { HttpRequestContext, RateLimitOptions } from '@putnami/application';
import { getEnv, type Logger } from '@putnami/runtime';
import { getCurrentProject } from '@putnami/utils';
import type { AnalyticsConfigValues } from './analytics.config';
import type { DeclaredEvents } from './declare';
import { createClientIpResolver } from './identity/client-ip';
import type { AnalyticsRuntime as IdentityRuntime } from './identity/identified-cookie';
import { createDedupCache, type DedupCache } from './sink/dedup-cache';
import { createWriteQueue, type WriteQueue } from './sink/queue';
import type { Sink } from './sink/sink';

/** The name stamped on rows when the application cannot name itself. */
export const FALLBACK_APP_NAME = 'app';

/** Raised when a request path reaches for a runtime the plugin never installed. */
export const NOT_INSTALLED_MESSAGE = 'analytics: plugin not installed';

/**
 * Everything one composed `analytics()` decided at warmup.
 *
 * It extends the identity seam rather than restating it, so
 * `resolveIdentifiedVisitor` takes the same three members and there is exactly
 * one `AnalyticsRuntime` in the package.
 *
 * Every value here is resolved once: the secret, the datasource, the declared
 * vocabulary, the known routes, the client-IP resolver. A request path that
 * had to re-resolve any of them would be re-reading configuration on the hot
 * path and could disagree with itself between two requests.
 */
export interface AnalyticsRuntime extends IdentityRuntime {
  /** The datasource the sink writes to, from `resolveAnalyticsDatasource`. */
  datasource: string;
  /** The application name stamped on every row. */
  app: string;
  /** The deployment environment (`test` | `production` | `local`). */
  env: string;
  /** `APP_VERSION`, or null when the build stamped none. */
  version: string | null;
  /** The declared action names the sanitizer filters the wire with. */
  declared: ReadonlySet<string>;
  /** The declared property schemas `validateProps` projects through. */
  declaredSchemas: DeclaredEvents;
  /** The public route of the tracker bundle, absent when none was built. */
  trackerUrl: string | undefined;
  /** The page routes a client event may name; anything else is `__unknown__`. */
  knownRoutes: ReadonlySet<string>;
  /** The ingest path, already carrying the owning module's base path. */
  endpoint: string;
  /** Where accepted rows go: Postgres, or the counting no-op sink. */
  sink: Sink;
  /**
   * The bounded queue every accepted row goes through.
   *
   * No response path ever awaits {@link Sink.write}. The queue is what turns a
   * slow database into a bounded memory cost instead of a slow page.
   */
  queue: WriteQueue;
  /** Resolves the client key of a request. The address is never stored. */
  clientIp: (ctx: HttpRequestContext) => string;
  /** The same trusted-proxy generator, handed to the ingest rate limiter. */
  keyGenerator: NonNullable<RateLimitOptions['keyGenerator']>;
  /** The per-instance recently-written event ids, shared with the sink. */
  dedup: DedupCache;
  /** Where the request path reports a sink failure. */
  logger: Logger;
  /** The clock, injected so every time-dependent proof is deterministic. */
  now: () => Date;
}

/** What {@link createRuntime} cannot derive on its own. */
export interface RuntimeInput {
  config: AnalyticsConfigValues;
  secret: string;
  datasource: string;
  declared: ReadonlySet<string>;
  declaredSchemas: DeclaredEvents;
  trackerUrl: string | undefined;
  knownRoutes: ReadonlySet<string>;
  endpoint: string;
  sink: Sink;
  /** The shared dedup cache; a fresh one is built when absent. */
  dedup?: DedupCache;
  logger: Logger;
  consent?: (ctx: HttpRequestContext) => boolean | Promise<boolean>;
  /** Overridden by tests; production reads the workspace project. */
  app?: string;
  /** Overridden by tests; production reads the clock. */
  now?: () => Date;
}

/**
 * Assembles the runtime the middleware, the ingest route, and `track()` share.
 *
 * @param input - What warmup resolved.
 * @returns The runtime.
 */
export function createRuntime(input: RuntimeInput): AnalyticsRuntime {
  const clientIp = createClientIpResolver(input.config.trustedProxies);
  const now = input.now ?? (() => new Date());
  return {
    config: input.config,
    secret: input.secret,
    consent: input.consent,
    datasource: input.datasource,
    app: input.app ?? resolveAppName(),
    env: getEnv(),
    version: process.env['APP_VERSION'] ?? null,
    declared: input.declared,
    declaredSchemas: input.declaredSchemas,
    trackerUrl: input.trackerUrl,
    knownRoutes: input.knownRoutes,
    endpoint: input.endpoint,
    sink: input.sink,
    queue: createWriteQueue({
      sink: input.sink,
      capacity: input.config.queueCapacity,
      batch: input.config.flushBatch,
      flushIntervalMs: input.config.flushIntervalMs,
      now,
      logger: input.logger,
    }),
    clientIp,
    // One generator instance, two consumers: the visitor hash keys on it and
    // the rate limiter buckets on it. Building a second one would let the two
    // disagree about which hop is the client — the exact seam an attacker
    // would probe to rotate a rate-limit bucket.
    keyGenerator: (ctx) => clientIp(ctx as unknown as HttpRequestContext),
    dedup: input.dedup ?? createDedupCache(),
    logger: input.logger,
    now,
  };
}

/**
 * The application name stamped on every row (body §C).
 *
 * `getCurrentProject()` already resolves the `package.json` name. An
 * application that cannot name itself still measures itself: refusing to serve
 * over a missing manifest would be the measurement breaking the application.
 *
 * @returns The project name, or `app`.
 */
export function resolveAppName(): string {
  try {
    const name = getCurrentProject().name;
    return typeof name === 'string' && name.length > 0 ? name : FALLBACK_APP_NAME;
  } catch {
    return FALLBACK_APP_NAME;
  }
}

let current: AnalyticsRuntime | undefined;

/**
 * Publishes (or clears) the process-wide runtime `track()` reads.
 *
 * One application per process — the serverless-first shape the framework is
 * built around — so a module-level slot is unambiguous. The plugin clears it
 * on `stop()` so a stopped application cannot keep answering.
 *
 * @param runtime - The runtime to publish, or undefined to clear.
 */
export function setAnalyticsRuntime(runtime: AnalyticsRuntime | undefined): void {
  current = runtime;
}

/**
 * Reads the published runtime.
 *
 * @returns The runtime the composed plugin installed.
 * @throws Error when no `analytics()` plugin is composed.
 */
export function analyticsRuntime(): AnalyticsRuntime {
  if (!current) {
    throw new Error(NOT_INSTALLED_MESSAGE);
  }
  return current;
}

/**
 * The default deadline of a manual flush, in milliseconds.
 *
 * Half of `SHUTDOWN_TIMEOUT_MS` in `@putnami/application`, which force-exits a
 * process whose `stop()` has not returned.
 */
export const FLUSH_DEADLINE_MS = 5000;

/**
 * Writes whatever the queue is still holding, or gives up at the deadline.
 *
 * Nothing on a response path needs this: rows land on their own, driven by the
 * elected request, the flush ticker, and the drain in `stop()`. It exists for
 * the two callers that need the queue empty *now* — a test asserting on rows it
 * has just produced, and an operator flushing before a maintenance stop.
 *
 * @param deadlineMs - The longest to wait before returning with rows still queued.
 * @returns A promise that resolves when the queue is empty or the deadline passed.
 */
export async function flushAnalytics(deadlineMs: number = FLUSH_DEADLINE_MS): Promise<void> {
  await current?.queue.drain(deadlineMs);
}

/**
 * The runtime, or `undefined` when analytics is absent or switched off.
 *
 * `analytics.enabled: false` is documented as an off switch that "registers
 * nothing", so a call site that measures must be able to ask without being
 * punished for the answer. Reserved for callers that no-op when there is
 * nothing to record; anything that cannot proceed without a runtime should use
 * {@link analyticsRuntime} and get the explanatory throw.
 *
 * @returns The published runtime, or undefined.
 */
export function optionalAnalyticsRuntime(): AnalyticsRuntime | undefined {
  return current;
}
