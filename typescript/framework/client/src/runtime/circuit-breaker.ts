import { incCounter, setGauge } from '@putnami/application';
import { getLogger } from '@putnami/utils';
import { ClientError } from './errors';
import { metricServiceName } from './metric-name';
import type { ClientRequest, ClientResponse, DisposableInterceptor, Interceptor } from './transport.type';

const logger = getLogger('client');

/** Numeric encoding of each circuit state for the `circuit.state` gauge. */
const STATE_GAUGE_VALUE: Record<CircuitState, number> = {
  closed: 0,
  open: 1,
  'half-open': 2,
};

/**
 * Circuit breaker states.
 */
export type CircuitState = 'closed' | 'open' | 'half-open';

/**
 * Circuit breaker configuration.
 */
export interface CircuitBreakerConfig {
  /** Number of failures before opening the circuit. Default: 5 */
  failureThreshold: number;
  /** Time in ms before trying again (half-open). Default: 30000 */
  resetTimeoutMs: number;
  /** Number of successful requests in half-open to close circuit. Default: 2 */
  successThreshold: number;
  /**
   * Maximum number of concurrent trial requests admitted in half-open state.
   * Extra requests are rejected with `CircuitOpenError` so a recovering service
   * is probed gradually instead of by a thundering herd. Default: 1
   */
  halfOpenMaxConcurrent: number;
  /** Optional health check URL to probe in open/half-open state */
  healthCheckUrl?: string;
  /** Interval in ms for health check probing when open. Default: 10000 */
  healthCheckIntervalMs: number;
  /** HTTP status codes that count as failures. Default: [500, 502, 503, 504] */
  failureStatuses: number[];
  /**
   * Service name used as the metric-key prefix for circuit telemetry
   * (`client.{serviceName}.circuit.state` gauge + `.circuit.transition.{state}`
   * counter). Pass the same name you give the telemetry interceptor so the
   * circuit metrics line up with the request metrics. When omitted, metrics are
   * emitted under `client.circuit.*`. Must match `[a-zA-Z0-9_-]+` to prevent
   * metric-key injection.
   */
  serviceName?: string;
}

const DEFAULT_CONFIG: CircuitBreakerConfig = {
  failureThreshold: 5,
  resetTimeoutMs: 30_000,
  successThreshold: 2,
  halfOpenMaxConcurrent: 1,
  healthCheckIntervalMs: 10_000,
  failureStatuses: [500, 502, 503, 504],
};

/**
 * Circuit breaker state machine.
 *
 * States:
 * - **closed**: Normal operation. Tracks consecutive failures.
 * - **open**: Fail-fast. All requests rejected immediately. Probes health if configured.
 * - **half-open**: Trial period. Allows limited requests to test if service recovered.
 */
export class CircuitBreaker {
  private state: CircuitState = 'closed';
  private failureCount = 0;
  private successCount = 0;
  /** Trial requests currently in flight while half-open (concurrency gate). */
  private halfOpenInFlight = 0;
  private lastFailureTime = 0;
  private healthCheckTimer: ReturnType<typeof setInterval> | undefined;
  private readonly config: CircuitBreakerConfig;
  /** Gauge key for the current circuit state. */
  private readonly stateGaugeKey: string;
  /** Prefix for the per-transition counter (`<prefix>{state}`). */
  private readonly transitionPrefix: string;

  constructor(config?: Partial<CircuitBreakerConfig>) {
    this.config = { ...DEFAULT_CONFIG, ...config };

    const serviceName = this.config.serviceName;
    const metricPrefix = serviceName ? `client.${metricServiceName(serviceName)}.circuit` : 'client.circuit';
    this.stateGaugeKey = `${metricPrefix}.state`;
    this.transitionPrefix = `${metricPrefix}.transition.`;

    // Validate healthCheckUrl scheme if provided
    if (this.config.healthCheckUrl) {
      try {
        const parsed = new URL(this.config.healthCheckUrl);
        if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
          throw new Error(`only http:// and https:// are allowed, got "${parsed.protocol}"`);
        }
      } catch (error) {
        throw new Error(`Invalid healthCheckUrl: ${error instanceof Error ? error.message : String(error)}`);
      }
    }
  }

  getState(): CircuitState {
    return this.state;
  }

  /**
   * Check if a request should be allowed through.
   *
   * In half-open state, admission is gated to `halfOpenMaxConcurrent` concurrent
   * trial requests; each admitted trial is counted as in-flight until a matching
   * `onSuccess`/`onFailure` is recorded.
   */
  allowRequest(): boolean {
    switch (this.state) {
      case 'closed':
        return true;
      case 'open': {
        // Check if enough time has passed to try again
        if (Date.now() - this.lastFailureTime >= this.config.resetTimeoutMs) {
          this.transitionTo('half-open');
          return this.admitHalfOpen();
        }
        return false;
      }
      case 'half-open':
        return this.admitHalfOpen();
    }
  }

  /**
   * Admit a trial request in half-open state if under the concurrency cap.
   * Counts the admitted request as in-flight.
   */
  private admitHalfOpen(): boolean {
    if (this.halfOpenInFlight >= this.config.halfOpenMaxConcurrent) {
      return false;
    }
    this.halfOpenInFlight++;
    return true;
  }

  /**
   * Record a successful request.
   */
  onSuccess(): void {
    switch (this.state) {
      case 'half-open':
        this.halfOpenInFlight = Math.max(0, this.halfOpenInFlight - 1);
        this.successCount++;
        if (this.successCount >= this.config.successThreshold) {
          this.transitionTo('closed');
        }
        break;
      case 'closed':
        // Reset failure count on success
        this.failureCount = 0;
        break;
    }
  }

  /**
   * Record a failed request.
   */
  onFailure(): void {
    this.lastFailureTime = Date.now();

    switch (this.state) {
      case 'closed':
        this.failureCount++;
        if (this.failureCount >= this.config.failureThreshold) {
          this.transitionTo('open');
        }
        break;
      case 'half-open':
        // Any failure in half-open goes back to open
        this.transitionTo('open');
        break;
    }
  }

  /** Release a half-open admission without treating a local/canceled call as service evidence. */
  onIgnored(): void {
    if (this.state === 'half-open') this.halfOpenInFlight = Math.max(0, this.halfOpenInFlight - 1);
  }

  /**
   * Start background health probing (when circuit is open).
   */
  startHealthProbing(): void {
    const healthCheckUrl = this.config.healthCheckUrl;
    if (!healthCheckUrl || this.healthCheckTimer) return;

    this.healthCheckTimer = setInterval(async () => {
      if (this.state !== 'open') {
        this.stopHealthProbing();
        return;
      }

      try {
        const response = await fetch(healthCheckUrl, {
          method: 'GET',
          signal: AbortSignal.timeout(5000),
        });
        if (response.ok) {
          this.transitionTo('half-open');
        }
      } catch (error) {
        logger.debug(
          `[circuit-breaker] Health probe failed for ${this.config.healthCheckUrl}: ${error instanceof Error ? error.message : String(error)}`,
        );
      }
    }, this.config.healthCheckIntervalMs);

    // Don't let the health-probe timer keep the event loop (process) alive.
    // `unref` exists on Node/Bun timer handles but not in the DOM lib types.
    (this.healthCheckTimer as { unref?: () => void }).unref?.();
  }

  stopHealthProbing(): void {
    if (this.healthCheckTimer) {
      clearInterval(this.healthCheckTimer);
      this.healthCheckTimer = undefined;
    }
  }

  private transitionTo(newState: CircuitState): void {
    this.state = newState;
    // Any state change clears the half-open trial gate.
    this.halfOpenInFlight = 0;

    // Emit resilience telemetry: a gauge with the current state and a counter
    // per transition. No-ops when telemetry is not initialized.
    setGauge(this.stateGaugeKey, STATE_GAUGE_VALUE[newState]);
    incCounter(`${this.transitionPrefix}${newState}`);

    switch (newState) {
      case 'closed':
        this.failureCount = 0;
        this.successCount = 0;
        this.stopHealthProbing();
        break;
      case 'open':
        this.successCount = 0;
        this.startHealthProbing();
        break;
      case 'half-open':
        this.successCount = 0;
        break;
    }
  }

  dispose(): void {
    this.stopHealthProbing();
  }
}

/**
 * Creates a circuit breaker interceptor.
 *
 * When the circuit is open, requests fail immediately with an error
 * instead of hitting the network. This prevents cascading failures
 * when a downstream service is down.
 *
 * If `healthCheckUrl` is configured (e.g. `http://service:3000/healthz`),
 * the circuit breaker probes it periodically and transitions to half-open
 * when the service responds OK.
 *
 * @example
 * ```typescript
 * const users = new UsersClient({
 *   baseUrl: 'http://users-api:3000',
 *   transport: 'http',
 *   interceptors: [
 *     circuitBreakerInterceptor({
 *       healthCheckUrl: 'http://users-api:3000/healthz',
 *       failureThreshold: 5,
 *       resetTimeoutMs: 30000,
 *     }),
 *   ],
 * });
 * ```
 *
 * The returned interceptor is disposable: it carries a `dispose()` that stops
 * the health-probe timer. {@link BaseClient} discovers this and tears it down
 * from `BaseClient.dispose()`, so callers usually never call it directly — but
 * if you use the interceptor outside a `BaseClient`, call `dispose()` to avoid
 * leaking the probe timer when `healthCheckUrl` is configured.
 */
export function circuitBreakerInterceptor(config?: Partial<CircuitBreakerConfig>): DisposableInterceptor {
  const breaker = new CircuitBreaker(config);
  // Resolve config once, not on every request
  const failureStatusSet = new Set({ ...DEFAULT_CONFIG, ...config }.failureStatuses);

  const interceptor: Interceptor = async (
    request: ClientRequest,
    next: (req: ClientRequest) => Promise<ClientResponse>,
  ) => {
    if (!breaker.allowRequest()) {
      throw new CircuitOpenError(breaker.getState());
    }

    try {
      const response = await next(request);

      if (failureStatusSet.has(response.status)) {
        breaker.onFailure();
      } else {
        breaker.onSuccess();
      }

      return response;
    } catch (error) {
      breaker.onFailure();
      throw error;
    }
  };

  return Object.assign(interceptor, { dispose: () => breaker.dispose() });
}

/**
 * Thrown when the circuit breaker is open and the request is rejected.
 */
export class CircuitOpenError extends ClientError {
  readonly code = 'client.circuit_open';
  readonly circuitState: CircuitState;

  constructor(state: CircuitState, service = '', method = '') {
    super({
      service,
      method,
      status: 0,
      message: `Circuit breaker is ${state} — request rejected to prevent cascading failure`,
    });
    this.name = 'CircuitOpenError';
    this.circuitState = state;
  }
}
