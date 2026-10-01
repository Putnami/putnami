import { useLogger } from '@putnami/runtime';
import type { Application } from './application';

/**
 * A factory that builds the application graph on demand.
 *
 * {@link bootstrapServe} invokes it *inside* its failure guard so that errors
 * thrown while constructing the graph — e.g. a synchronous `useConfig()` whose
 * config source fails — are caught and logged identically to errors thrown by
 * `start()`.
 */
export type AppFactory = () => Application | Promise<Application>;

/**
 * Startup errors already logged by {@link Application.start}'s own catch.
 * Tracked so the bootstrap guard does not emit a *second* `‼️ startup failed`
 * line for a start()-phase failure: start() logs the error, then re-throws it
 * to us. Construction-phase errors never pass through start(), so they are not
 * in this set and the guard logs them itself.
 */
const startupFailuresLogged = new WeakSet<object>();

/**
 * The most recently logged *primitive* startup failure. Primitives (a thrown
 * string, number, etc.) cannot be held in a WeakSet, so they are tracked in a
 * single slot: startup happens once per process before it exits, and start()
 * re-throws synchronously into the guard's catch, so the last-marked primitive
 * is exactly the one the guard is about to inspect. Wrapped in a box so a thrown
 * `undefined` is still distinguishable from "nothing marked yet".
 */
let lastLoggedPrimitiveFailure: { readonly value: unknown } | undefined;

const isObject = (error: unknown): error is object => typeof error === 'object' && error !== null;

/**
 * Record that a startup failure has already been logged, so a downstream guard
 * (see {@link bootstrapServe}) does not log it a second time.
 * @internal
 */
export function markStartupFailureLogged(error: unknown): void {
  if (isObject(error)) {
    startupFailuresLogged.add(error);
  } else {
    lastLoggedPrimitiveFailure = { value: error };
  }
}

function startupFailureAlreadyLogged(error: unknown): boolean {
  if (isObject(error)) {
    return startupFailuresLogged.has(error);
  }
  return lastLoggedPrimitiveFailure !== undefined && Object.is(lastLoggedPrimitiveFailure.value, error);
}

/**
 * Top-level serve guard for generated and hand-written entrypoints.
 *
 * Builds the application from `factory` and starts it, converting **any** fatal
 * startup failure — thrown while constructing the app graph *or* while starting
 * plugins — into a single structured log line via the active logger (one JSON
 * line on Cloud Run, where `JsonSink` is auto-enabled), then exits non-zero.
 *
 * Without this guard the failure escapes to the runtime's default handler, which
 * prints a multi-line stack trace. On a line-oriented log collector (Cloud Run →
 * Cloud Logging) each of those lines becomes a separate, severity-less entry,
 * fragmenting one crash across many records. Routing through the logger keeps a
 * fatal crash to exactly one queryable `ERROR` entry.
 *
 * Passing the *factory* (not an already-constructed `Application`) is
 * deliberate: it pulls graph construction inside the guard, so config/DI
 * failures that surface during `app()` are handled the same as `start()`
 * failures — the exact case that otherwise escapes.
 *
 * @param factory Builds the application graph (typically the `app` export).
 */
export async function bootstrapServe(factory: AppFactory): Promise<void> {
  try {
    const app = await factory();
    await app.start();
  } catch (error) {
    if (!startupFailureAlreadyLogged(error)) {
      const detail = error instanceof Error ? error.message : String(error);
      useLogger('putnami').error(`‼️ startup failed: ${detail}`, error);
    }
    // A serverless workload must exit decisively on a failed boot: a hung
    // container is worse than an aborted log flush. Mirror the hard exit used
    // by installSignalHandlers rather than re-throwing into the default printer.
    process.exit(1);
  }
}
