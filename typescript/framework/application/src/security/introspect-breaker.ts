/**
 * In-process circuit breaker for the RFC 7662 introspection strategy.
 *
 * This is the TypeScript twin of `go/framework/security/introspect_breaker.go`
 * — a classic closed → open → half-open breaker guarding the upstream
 * `/introspect` call:
 *
 *   - closed: fewer than `failureThreshold` consecutive failures; calls pass
 *     through.
 *   - open: threshold reached; `allow()` denies calls for `openDurationMs`, so an
 *     uncached token fails closed IMMEDIATELY instead of stalling on a timeout
 *     against a dead endpoint. Cached results are unaffected — the breaker only
 *     gates the network, and the cache lookup happens before `allow()` is
 *     consulted.
 *   - half-open: after `openDurationMs`, exactly one trial call is admitted
 *     (`trialInFlight` serializes it). Its `record()` closes the breaker on
 *     success or re-opens it on failure; a throttled trial `release()`s the slot
 *     instead, so the next caller probes again.
 *
 * A 429 Too Many Requests is neither a success nor a failure: `release()` frees
 * a half-open probe slot without touching the failure run, so backpressure from
 * a live endpoint can never open the breaker, reset a genuine failure run, or
 * wedge it half-open.
 *
 * It is deliberately in-process only (no `@putnami/client`, no shared network
 * state): the blast radius of an introspection outage is one instance, and a
 * distributed breaker would add a network dependency to the very path that is
 * failing.
 */

/**
 * How long the breaker stays open before probing the endpoint with a single
 * trial request, when the caller enables the breaker without setting an open
 * duration. Mirrors Go's `defaultBreakerOpenDuration` (30s).
 */
export const DEFAULT_BREAKER_OPEN_DURATION_MS = 30_000;

/** Configures the introspection circuit breaker (see {@link CircuitBreaker}). */
export interface BreakerConfig {
  /**
   * Number of consecutive upstream failures (transport error, timeout, or a
   * non-200 status including rejected client credentials, but never a 429 Too
   * Many Requests) that opens the breaker. Zero or negative disables the
   * breaker entirely.
   */
  failureThreshold?: number;

  /**
   * How long the breaker stays open — failing uncached tokens closed without an
   * upstream call — before letting a single trial request through. Defaults to
   * 30s when `failureThreshold > 0`.
   */
  openDurationMs?: number;
}

/** The outcome of {@link CircuitBreaker.allow}. */
export interface BreakerDecision {
  /** Whether an upstream introspection call may proceed now. */
  allowed: boolean;
  /**
   * Whether this caller is the single half-open trial. The caller MUST pass this
   * value back to {@link CircuitBreaker.record} or {@link CircuitBreaker.release}
   * so a granted trial slot is always released.
   */
  trial: boolean;
}

export class CircuitBreaker {
  private readonly threshold: number;
  private readonly openForMs: number;
  private readonly now: () => number;

  private failures = 0;
  /** Epoch millis when the breaker last opened; 0 means it has never opened. */
  private openedAt = 0;
  private trialInFlight = false;

  private constructor(threshold: number, openForMs: number, now: () => number) {
    this.threshold = threshold;
    this.openForMs = openForMs;
    this.now = now;
  }

  /**
   * Returns a breaker for the config, or `undefined` when the breaker is disabled
   * (`failureThreshold <= 0`) so the hot path can skip it entirely — an
   * unconfigured breaker adds zero behavior change (mirrors Go's
   * `newCircuitBreaker` returning nil).
   *
   * `now` is injectable so the state machine can be exercised deterministically
   * in tests; production uses the wall clock.
   */
  static create(cfg: BreakerConfig | undefined, now: () => number = () => Date.now()): CircuitBreaker | undefined {
    const threshold = cfg?.failureThreshold ?? 0;
    if (threshold <= 0) {
      return undefined;
    }
    const configured = cfg?.openDurationMs ?? 0;
    const openForMs = configured > 0 ? configured : DEFAULT_BREAKER_OPEN_DURATION_MS;
    return new CircuitBreaker(threshold, openForMs, now);
  }

  /**
   * Reports whether an upstream introspection call may proceed now. When the
   * breaker is open it denies until `openDurationMs` has elapsed, then admits a
   * single trial (`trial: true`). The caller MUST call {@link record} or
   * {@link release} with the same `trial` value once the admitted call
   * completes.
   */
  allow(): BreakerDecision {
    if (this.failures < this.threshold) {
      return { allowed: true, trial: false }; // closed
    }
    if (this.now() - this.openedAt < this.openForMs) {
      return { allowed: false, trial: false }; // open: fail closed without a network call
    }
    if (this.trialInFlight) {
      return { allowed: false, trial: false }; // a trial is already probing the endpoint
    }
    this.trialInFlight = true;
    return { allowed: true, trial: true }; // half-open: this caller probes the endpoint
  }

  /**
   * Folds one upstream call's outcome into the breaker state. `success` means the
   * endpoint responded (a 200 — active or inactive — the endpoint is healthy);
   * any transport error, timeout, or non-200 status other than 429 is a failure
   * (a 429 goes to {@link release} instead). `trial` must be the value
   * {@link allow} returned for this call, so the half-open probe slot is freed.
   */
  record(success: boolean, trial: boolean): void {
    if (trial) {
      this.trialInFlight = false;
    }
    if (success) {
      this.failures = 0;
      this.openedAt = 0;
      return;
    }
    this.failures++;
    if (this.failures >= this.threshold) {
      // Cross (or re-cross) the threshold: arm the full open window. A trial
      // failure lands here too, so a still-dead endpoint re-opens for the window.
      this.openedAt = this.now();
    }
  }

  /**
   * Frees a granted half-open probe slot WITHOUT folding an outcome into the
   * breaker: the failure run and the open window stay untouched, so the call
   * neither resets nor extends a failure run, and an open window is neither
   * closed nor re-armed. It is the completion for a 429 Too Many Requests —
   * backpressure from a live endpoint, not an outage. `trial` must be the value
   * {@link allow} returned for this call; `release(false)` is a no-op.
   */
  release(trial: boolean): void {
    if (trial) {
      this.trialInFlight = false;
    }
  }
}
