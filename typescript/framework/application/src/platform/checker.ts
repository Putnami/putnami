import type { Promisable } from '@putnami/utils';

/**
 * Implemented by plugins that probe the liveness of a dependency they
 * own — a connection pool, a cache, an upstream service. The platform
 * plugin walks the module tree at warmup and registers every checker
 * under its `name`.
 *
 * Use HealthChecker for "dependency is broken in a way only a restart
 * fixes" — Kubernetes liveness restarts the pod when this fails. For
 * "dependency is temporarily down, drain traffic but don't restart",
 * implement {@link ReadinessChecker} instead.
 *
 * Implementations MUST be safe to call concurrently and MUST honour
 * `AbortSignal` cancellation. Failures surface as their `Error.message`
 * verbatim in the /healthz response — operators read these in alert
 * pages, so runtimes must not wrap or rewrite.
 */
export interface HealthChecker {
  /** Probe identifier; surfaces as the JSON key in /healthz checks. */
  readonly name: string;
  checkHealth(signal: AbortSignal): Promisable<void>;
}

/**
 * Implemented by plugins that report whether they are ready to serve
 * traffic — caches warmed, downstream connections established, leader
 * election complete, etc.
 *
 * Use ReadinessChecker for transient warmup / dependency-flapping
 * states. Kubernetes readiness drains traffic without restarting.
 *
 * Same concurrency and cancellation rules as {@link HealthChecker}.
 */
export interface ReadinessChecker {
  readonly name: string;
  checkReadiness(signal: AbortSignal): Promisable<void>;
}

/** Runtime type guard for {@link HealthChecker}. */
export function isHealthChecker(value: unknown): value is HealthChecker {
  return (
    typeof value === 'object' &&
    value !== null &&
    'checkHealth' in value &&
    typeof (value as HealthChecker).checkHealth === 'function' &&
    typeof (value as HealthChecker).name === 'string'
  );
}

/** Runtime type guard for {@link ReadinessChecker}. */
export function isReadinessChecker(value: unknown): value is ReadinessChecker {
  return (
    typeof value === 'object' &&
    value !== null &&
    'checkReadiness' in value &&
    typeof (value as ReadinessChecker).checkReadiness === 'function' &&
    typeof (value as ReadinessChecker).name === 'string'
  );
}
