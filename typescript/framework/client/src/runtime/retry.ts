import { incCounter } from '@putnami/application';
import { ClientError, ClientFrameworkError, ClientRetryExhaustedError } from './errors';
import { metricServiceName } from './metric-name';
import { FIRST_PARTY_UNARY_DEFAULTS, resolveClientResilience } from './service-resilience';
import type { ClientRequest, ClientResponse, Interceptor } from './transport.type';

/** Hard ceiling on retry attempts to prevent retry-storm misconfiguration. */
export const MAX_RETRIES_CAP = 10;

/**
 * Retry configuration.
 */
export interface RetryConfig {
  /** Maximum number of retry attempts (0 = no retries). Default: 3 */
  maxRetries: number;
  /** Base delay between retries in ms. Default: 200 */
  baseDelayMs: number;
  /** Maximum delay between retries in ms. Default: 5000 */
  maxDelayMs: number;
  /** HTTP status codes that are retryable. Default: [429, 502, 503, 504] */
  retryableStatuses: number[];
  /** Whether to add random jitter to the delay. Default: true */
  jitter: boolean;
}

const DEFAULT_RETRY_CONFIG: RetryConfig = {
  maxRetries: 3,
  baseDelayMs: 200,
  maxDelayMs: 5000,
  retryableStatuses: [429, 502, 503, 504],
  jitter: true,
};

/**
 * Options controlling per-attempt behaviour layered on top of {@link RetryConfig}.
 */
interface RetryOptions {
  /**
   * Per-attempt request timeout in ms. When set, each attempt gets a fresh
   * `AbortSignal.timeout`, combined with the caller's cancel signal.
   *
   * Note: this is a *per-attempt* budget. Total wall-clock latency across all
   * retries (attempts + backoff) is bounded separately by {@link maxElapsedMs}.
   */
  timeoutMs?: number;
  /**
   * Overall deadline in ms for the whole retry sequence (attempts + backoff),
   * measured from the first attempt. Defaults to {@link timeoutMs} when that is
   * set, so retries can never amplify worst-case latency beyond a single
   * `timeoutMs`. Set explicitly to allow a larger total budget than one attempt.
   *
   * When the deadline is reached the interceptor stops retrying and throws
   * `ClientRetryExhaustedError` (or the last underlying error). Each attempt's
   * timeout is clamped to `min(timeoutMs, remaining)` and any backoff sleep that
   * would overrun the deadline is skipped.
   *
   * Only takes effect when {@link timeoutMs} is also set.
   */
  maxElapsedMs?: number;
  /**
   * Optional accessor for the service identifier used in
   * `ClientRetryExhaustedError`. Read lazily so subclass fields initialized
   * after `BaseClient` construction are visible.
   */
  getServiceName?: () => string;
}

/**
 * Compute the delay for a given attempt using exponential backoff with optional jitter.
 */
export function computeDelay(attempt: number, config: RetryConfig): number {
  const exponential = config.baseDelayMs * 2 ** attempt;
  const capped = Math.min(exponential, config.maxDelayMs);
  if (config.jitter) {
    return Math.round(capped * (0.5 + Math.random() * 0.5));
  }
  return capped;
}

/**
 * Read a provider `Retry-After` header as a delay in milliseconds, accepting both
 * forms RFC 9110 defines: delta-seconds and an HTTP-date. Returns `undefined`
 * when the header is absent or not one of those forms — a value this client
 * cannot read is never turned into a guessed delay. A date already in the past
 * reads as `0`: the provider is asking for an immediate retry, not a negative wait.
 */
export function parseRetryAfter(value: string | null | undefined, now = Date.now()): number | undefined {
  if (value === null || value === undefined) return undefined;
  const text = value.trim();
  if (!text) return undefined;
  if (/^\d+$/.test(text)) {
    const seconds = Number(text);
    return Number.isSafeInteger(seconds) ? seconds * 1000 : undefined;
  }
  // Anything else must be an HTTP-date. Reject numeric-looking text outright:
  // `Date.parse` accepts `-3` and `1e3` as years, which would turn a malformed
  // delta-seconds value into a silent multi-year wait.
  if (/^[+-]?[\d.eE+-]+$/.test(text)) return undefined;
  const at = Date.parse(text);
  if (Number.isNaN(at)) return undefined;
  return Math.max(0, at - now);
}

/**
 * Returns true if the error or status is retryable.
 */
function isRetryable(status: number, retryableSet: ReadonlySet<number>): boolean {
  return retryableSet.has(status);
}

function isNetworkError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  // AbortError = manual cancellation, TimeoutError = AbortSignal.timeout() — don't retry either
  if (error.name === 'AbortError' || error.name === 'TimeoutError') return false;
  // TypeError from fetch = DNS failure, connection refused, etc. — retryable
  if (error instanceof TypeError) return true;
  return false;
}

/**
 * Creates a retry interceptor that wraps requests with exponential backoff.
 * Only retries on network errors and retryable HTTP status codes.
 * Does not retry on client errors (4xx except 429).
 *
 * Network-error exhaustion throws {@link ClientRetryExhaustedError}. When
 * `options.timeoutMs` is set, each attempt gets its own timeout budget and a
 * per-attempt timeout is itself retryable.
 *
 * Total wall-clock latency is bounded by an overall deadline derived from
 * `options.maxElapsedMs` (defaulting to `options.timeoutMs`), so retries can
 * never amplify the worst case beyond the configured budget: each attempt's
 * timeout is clamped to the remaining budget, backoff sleeps that would overrun
 * it are skipped, and the deadline being reached ends the sequence.
 */
export function retryInterceptor(
  config?: Partial<RetryConfig>,
  optionsOrGetServiceName?: RetryOptions | (() => string),
): Interceptor {
  const options =
    typeof optionsOrGetServiceName === 'function'
      ? { getServiceName: optionsOrGetServiceName }
      : optionsOrGetServiceName;
  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: retry state, deadlines, and typed exhaustion stay in one attempt loop
  return async (request: ClientRequest, next: (req: ClientRequest) => Promise<ClientResponse>) => {
    const firstParty = request.clientOperation ? resolveClientResilience(request) : undefined;
    const merged: RetryConfig = firstParty
      ? {
          maxRetries: firstParty.retry.maxAttempts - 1,
          baseDelayMs: FIRST_PARTY_UNARY_DEFAULTS.retryBaseDelayMs,
          maxDelayMs: FIRST_PARTY_UNARY_DEFAULTS.retryMaxDelayMs,
          retryableStatuses: [...firstParty.retry.statuses],
          jitter: true,
        }
      : { ...DEFAULT_RETRY_CONFIG, ...config };
    const resolved: RetryConfig = {
      ...merged,
      maxRetries: request.streamedRequest ? 0 : resolveMaxRetries(merged.maxRetries),
    };
    const retryableSet = new Set(resolved.retryableStatuses);
    const retryableCodes = new Set(firstParty?.retry.codes ?? []);
    const timeoutMs = firstParty?.attemptTimeoutMs ?? options?.timeoutMs;
    const maxElapsedMs =
      firstParty?.timeoutMs ??
      (timeoutMs !== undefined ? (options?.maxElapsedMs !== undefined ? options.maxElapsedMs : timeoutMs) : undefined);
    const callerSignal = request.signal;
    const deadline = request.deadlineAt ?? (maxElapsedMs !== undefined ? Date.now() + maxElapsedMs : undefined);
    let lastError: Error | undefined;

    // Retry telemetry, mirroring the `client.{service}.circuit.*` family in
    // circuit-breaker.ts: a low-cardinality metric prefix plus dotted members.
    // Service name is resolved lazily (subclass fields aren't set yet at
    // BaseClient construction) and falls back to `client.retry` without a name.
    // All counters no-op when telemetry is not initialized.
    //  - `.attempt`   — one per retry performed (retry amplification, otherwise
    //                   invisible since the telemetry interceptor sits outside
    //                   retry and sees the whole sequence as one request).
    //  - `.exhausted` — the sequence gave up after retrying (threw, or returned a
    //                   still-retryable response it could no longer retry).
    //  - `.succeeded` — the request recovered: returned a non-retryable response
    //                   after at least one retry.
    const service = options?.getServiceName?.();
    const metricPrefix = service ? `client.${metricServiceName(service)}.retry` : 'client.retry';
    let retried = false;
    const recordRetry = (): void => {
      retried = true;
      incCounter(`${metricPrefix}.attempt`);
    };
    const recordExhausted = (): void => incCounter(`${metricPrefix}.exhausted`);
    const recordSucceeded = (): void => incCounter(`${metricPrefix}.succeeded`);

    const exhaustedError = (attempts: number, err: Error): ClientRetryExhaustedError =>
      new ClientRetryExhaustedError({
        service: options?.getServiceName?.() ?? '',
        method: `${request.method} ${request.path}`,
        attempts,
        lastError: err,
      });

    // A provider that states its own backoff wins over the computed one: the
    // client waits exactly as long as it was asked to. The remaining budget still
    // caps it — a hint the deadline cannot absorb ends the sequence instead of
    // being shortened, because retrying earlier than asked is what the header
    // exists to prevent.
    const retryDelay = (currentAttempt: number, retryAfterMs?: number): number | undefined => {
      const delay = retryAfterMs ?? computeDelay(currentAttempt, resolved);
      if (deadline !== undefined && Date.now() + delay >= deadline) {
        return undefined;
      }
      return delay;
    };

    try {
      for (let attempt = 0; attempt <= resolved.maxRetries; attempt++) {
        // Clamp each attempt's timeout to whatever budget remains so the whole
        // sequence stays within the overall deadline.
        const remaining = deadline !== undefined ? deadline - Date.now() : undefined;
        // No budget left for a fresh attempt — the deadline ended the sequence. A
        // backoff sleep overshooting its target lands us here instead of the
        // retryDelay()-gated path below, so surface the same typed error when the
        // sequence was network-error driven (raw error otherwise, e.g. a status
        // sequence that returns its last response).
        if (attempt > 0 && remaining !== undefined && remaining <= 0) {
          if (lastError && isNetworkError(lastError)) {
            throw exhaustedError(attempt, lastError);
          }
          break;
        }
        const attemptTimeoutMs =
          timeoutMs !== undefined && remaining !== undefined ? Math.max(0, Math.min(timeoutMs, remaining)) : timeoutMs;
        const attemptTimeout = attemptTimeoutMs !== undefined ? AbortSignal.timeout(attemptTimeoutMs) : undefined;
        const attemptSignal = combineSignals(callerSignal, attemptTimeout);
        const telemetryState = request.telemetryState ?? { attempts: 0 };
        telemetryState.attempts = attempt + 1;
        const retryState: { retryAfterMs?: number } = {};
        const attemptRequest = {
          ...request,
          signal: attemptSignal,
          telemetryState,
          retryState,
          telemetryAttempt: attempt + 1,
        };

        try {
          // biome-ignore lint/performance/noAwaitInLoops: attempts must execute sequentially
          const response = await next(attemptRequest);

          if (
            attempt < resolved.maxRetries &&
            isRetryable(response.status, retryableSet) &&
            !isDeclaredNonRetryable(request.clientOperation, response.status)
          ) {
            const delay = retryDelay(attempt, retryState.retryAfterMs);
            if (delay !== undefined) {
              lastError = new Error(`HTTP ${response.status}`);
              recordRetry();
              await sleep(delay, callerSignal);
              continue;
            }
          }

          // Terminal success return. When we retried at least once, distinguish a
          // genuine recovery from giving up on a still-retryable response.
          if (retried) {
            if (
              isRetryable(response.status, retryableSet) &&
              !isDeclaredNonRetryable(request.clientOperation, response.status)
            ) {
              recordExhausted();
            } else {
              recordSucceeded();
            }
          }
          return response;
        } catch (error) {
          if (callerSignal?.aborted) {
            throw error;
          }

          const err = error instanceof Error ? error : new Error(String(error));

          if (attemptTimeout?.aborted) {
            if (attempt < resolved.maxRetries) {
              const delay = retryDelay(attempt, retryState.retryAfterMs);
              if (delay !== undefined) {
                lastError = err;
                recordRetry();
                await sleep(delay, callerSignal);
                continue;
              }
            }
            throw error;
          }

          if (
            attempt < resolved.maxRetries &&
            (isNetworkError(error) ||
              isRetryableClientError(error, request.clientOperation, retryableSet, retryableCodes))
          ) {
            const delay = retryDelay(attempt, retryState.retryAfterMs);
            if (delay !== undefined) {
              lastError = err;
              recordRetry();
              await sleep(delay, callerSignal);
              continue;
            }
          }

          // Network-error retries exhausted (we retried at least once) → typed error.
          if (lastError && isNetworkError(error)) {
            throw exhaustedError(attempt + 1, err);
          }

          throw error;
        }
      }

      // Fell out of the loop without returning (e.g. the deadline ended the
      // sequence) — the outer catch records the exhausted outcome.
      throw lastError ?? new Error('Retry exhausted');
    } catch (error) {
      // Any terminal throw after we retried at least once is a give-up. The
      // in-loop `return response` path bypasses this (it records its own
      // outcome), so success is never double-counted.
      if (retried) recordExhausted();
      throw error;
    }
  };
}

/**
 * A status the operation declares non-retryable is never retried, whatever the
 * generic retryable-status set or a provider `Retry-After` header says. The
 * declaration is the contract; the status set is only a default.
 */
function isDeclaredNonRetryable(operation: ClientRequest['clientOperation'], status: number): boolean {
  const declared = operation?.errors.filter((entry) => entry.status === status) ?? [];
  return declared.length > 0 && declared.every((entry) => entry.retryable === false);
}

function isRetryableClientError(
  error: unknown,
  operation: ClientRequest['clientOperation'],
  statuses: ReadonlySet<number>,
  codes: ReadonlySet<string>,
): boolean {
  if (!(error instanceof ClientError)) return false;
  if (error instanceof ClientFrameworkError) {
    const declared = operation?.errors.find((entry) => entry.code === error.code && entry.status === error.status);
    if (declared?.retryable === false) return false;
    if (declared?.retryable === true) return true;
  }
  if (statuses.has(error.status)) return true;
  return error instanceof ClientFrameworkError && codes.has(error.code);
}

/**
 * Combine a caller cancel signal with a per-attempt timeout signal.
 * Returns the single non-undefined signal when only one is present.
 */
function combineSignals(caller?: AbortSignal, attempt?: AbortSignal): AbortSignal | undefined {
  if (caller && attempt) return AbortSignal.any([caller, attempt]);
  return caller ?? attempt;
}

function resolveMaxRetries(maxRetries: number): number {
  if (!Number.isFinite(maxRetries)) return 0;
  return Math.min(Math.max(Math.trunc(maxRetries), 0), MAX_RETRIES_CAP);
}

/**
 * Signal-aware sleep — resolves after ms or rejects immediately if signal fires.
 */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      'abort',
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}
