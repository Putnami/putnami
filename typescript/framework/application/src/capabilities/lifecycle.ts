import type { LifecyclePhase } from './manifest.types';

/**
 * A named lifecycle hook a plugin contributes to the capability manifest.
 *
 * TypeScript plugins expose `start()`/`stop()` methods, but those are anonymous
 * (no stable name) and every plugin has them, so they are not a faithful analog
 * of Go's named `Starter`/`Stopper` lifecycle components. A plugin that owns a
 * lifecycle resource worth recording (a connection pool, a scheduler, a
 * background worker) declares it explicitly via {@link LifecycleContributor} so
 * the capabilities producer can stamp it with a name and phase — the direct
 * counterpart of the Go emitter's `Collect[Starter]`/`Collect[Stopper]` walk.
 */
export interface LifecycleContribution {
  /** Stable hook identifier surfaced in the manifest. */
  readonly name: string;
  /** Whether the hook runs on startup (`starter`) or shutdown (`stopper`). */
  readonly phase: LifecyclePhase;
}

/**
 * Implemented by plugins that own named lifecycle hooks. The capabilities
 * producer walks every `LifecycleContributor` in the module tree and records
 * its contributions as manifest `lifecycleHooks`.
 *
 * @example
 * ```typescript
 * class ConnectionPoolPlugin implements Plugin, LifecycleContributor {
 *   name = 'connectionPool';
 *   lifecycleContributions(): LifecycleContribution[] {
 *     return [
 *       { name: 'connectionPool', phase: 'starter' },
 *       { name: 'connectionPool', phase: 'stopper' },
 *     ];
 *   }
 * }
 * ```
 */
export interface LifecycleContributor {
  lifecycleContributions(): LifecycleContribution[];
}

/**
 * Runtime type guard for {@link LifecycleContributor}. The capabilities producer
 * uses this to filter the plugin list when collecting lifecycle hooks.
 */
export function isLifecycleContributor(value: unknown): value is LifecycleContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'lifecycleContributions' in value &&
    typeof (value as LifecycleContributor).lifecycleContributions === 'function'
  );
}
