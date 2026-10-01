import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ClientRetryExhaustedError } from '../../src/runtime/errors';
import { MAX_RETRIES_CAP, retryInterceptor } from '../../src/runtime/retry';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function makeRequest(signal?: AbortSignal): ClientRequest {
  return { method: 'GET', path: '/test', headers: new Headers(), signal };
}

function makeResponse(status: number): ClientResponse {
  return { data: { ok: true }, status, headers: new Headers() };
}

/**
 * A `next` that mimics fetch: it rejects with a TimeoutError as soon as the
 * per-attempt signal aborts. Used to prove each attempt has its own deadline.
 */
function slowNext(behavior: (attempt: number) => 'timeout' | ClientResponse) {
  let attempt = 0;
  return (req: ClientRequest): Promise<ClientResponse> => {
    const current = attempt++;
    const outcome = behavior(current);
    if (outcome !== 'timeout') {
      return Promise.resolve(outcome);
    }
    return new Promise<ClientResponse>((_resolve, reject) => {
      const signal = req.signal;
      if (!signal) {
        reject(new Error('expected a per-attempt signal'));
        return;
      }
      signal.addEventListener('abort', () => reject(new DOMException('The operation timed out', 'TimeoutError')), {
        once: true,
      });
    });
  };
}

describe('retryInterceptor per-attempt timeout', () => {
  specTest(
    'gives each attempt its own fresh timeout budget',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'each-attempt-gets-its-own-fresh-timeout-budget',
    },
    async () => {
      let calls = 0;
      // maxElapsedMs is generous so the overall deadline does not curtail the
      // three per-attempt timeouts this test exercises.
      const interceptor = retryInterceptor(
        { maxRetries: 2, baseDelayMs: 1, jitter: false },
        { timeoutMs: 20, maxElapsedMs: 10_000 },
      );

      const next = slowNext((attempt) => {
        calls = attempt + 1;
        // First two attempts time out; the third returns quickly.
        return attempt < 2 ? 'timeout' : makeResponse(200);
      });

      const start = Date.now();
      const result = await interceptor(makeRequest(), next);
      const elapsed = Date.now() - start;

      expect(result.status).toBe(200);
      expect(calls).toBe(3); // 1 initial + 2 retries, each with its own timeout
      // A single shared 20ms budget could never span three ~20ms attempts; the
      // fact that we made it to attempt 3 proves the budget is per-attempt.
      expect(elapsed).toBeGreaterThanOrEqual(35);
    },
  );

  specTest(
    'per-attempt timeout is retried (treated as transient)',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'a-per-attempt-timeout-is-treated-as-transient-and-retried',
    },
    async () => {
      let calls = 0;
      // Generous overall budget so the per-attempt-timeout retry path is reached
      // for every attempt rather than being cut short by the deadline.
      const interceptor = retryInterceptor(
        { maxRetries: 3, baseDelayMs: 1, jitter: false },
        { timeoutMs: 10, maxElapsedMs: 10_000 },
      );

      const next = slowNext((attempt) => {
        calls = attempt + 1;
        return 'timeout'; // every attempt times out
      });

      await expect(interceptor(makeRequest(), next)).rejects.toBeInstanceOf(DOMException);
      expect(calls).toBe(4); // 1 initial + 3 retries, all timed out then exhausted
    },
  );

  specTest(
    'caller cancellation during an attempt is not retried',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'a-caller-cancellation-during-an-attempt-is-not-retried',
    },
    async () => {
      const controller = new AbortController();
      const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1, jitter: false }, { timeoutMs: 1000 });

      let calls = 0;
      const next = (req: ClientRequest): Promise<ClientResponse> => {
        calls++;
        return new Promise<ClientResponse>((_resolve, reject) => {
          req.signal?.addEventListener('abort', () => reject(req.signal?.reason), { once: true });
        });
      };

      const reason = new Error('caller-cancelled');
      const pending = interceptor(makeRequest(controller.signal), next);
      setTimeout(() => controller.abort(reason), 5);

      await expect(pending).rejects.toBe(reason);
      expect(calls).toBe(1); // caller cancel wins, no retry
    },
  );

  specTest(
    'without timeoutMs no per-attempt signal is injected',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'no-per-attempt-signal-is-injected-without-a-timeout',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 1, baseDelayMs: 1 });
      let sawSignal = false;

      await interceptor(makeRequest(), async (req) => {
        sawSignal = req.signal !== undefined;
        return makeResponse(200);
      });

      expect(sawSignal).toBe(false);
    },
  );

  specTest(
    'combines caller signal with the per-attempt timeout',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'the-caller-signal-is-combined-with-the-per-attempt-timeout',
    },
    async () => {
      const controller = new AbortController();
      const interceptor = retryInterceptor({ maxRetries: 0 }, { timeoutMs: 1000 });
      let signalSeen: AbortSignal | undefined;

      await interceptor(makeRequest(controller.signal), async (req) => {
        signalSeen = req.signal;
        return makeResponse(200);
      });

      // A combined signal is present and is not the caller signal verbatim.
      expect(signalSeen).toBeDefined();
      expect(signalSeen).not.toBe(controller.signal);
    },
  );
});

describe('retryInterceptor overall deadline', () => {
  specTest(
    'bounds total elapsed time to ~timeoutMs even when every attempt stalls',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'total-elapsed-time-is-bounded-by-the-timeout',
    },
    async () => {
      // Each attempt stalls until its signal aborts; with the old per-attempt-only
      // budget this would run for ~maxRetries × timeoutMs. The default overall
      // deadline (= timeoutMs) must keep the whole sequence within one timeout.
      const interceptor = retryInterceptor(
        { maxRetries: 3, baseDelayMs: 1, jitter: false },
        { timeoutMs: 40 }, // maxElapsedMs defaults to timeoutMs
      );

      const next = slowNext(() => 'timeout'); // every attempt times out

      const start = Date.now();
      await expect(interceptor(makeRequest(), next)).rejects.toThrow();
      const elapsed = Date.now() - start;

      // A naive per-attempt timeout would allow up to 4 × 40ms = 160ms plus
      // backoff. With the overall deadline the total stays close to a single
      // timeoutMs; allow generous scheduler slack.
      expect(elapsed).toBeLessThan(120);
    },
  );

  specTest(
    'a separate maxElapsedMs caps total time across multiple attempts',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-separate-max-elapsed-caps-total-time-across-attempts',
    },
    async () => {
      let calls = 0;
      const interceptor = retryInterceptor(
        { maxRetries: 10, baseDelayMs: 1, jitter: false },
        { timeoutMs: 15, maxElapsedMs: 60 },
      );

      const next = slowNext((attempt) => {
        calls = attempt + 1;
        return 'timeout';
      });

      const start = Date.now();
      await expect(interceptor(makeRequest(), next)).rejects.toThrow();
      const elapsed = Date.now() - start;

      // Total bounded by maxElapsedMs (≈4 × 15ms attempts), not 11 × 15ms.
      expect(elapsed).toBeLessThan(140);
      expect(calls).toBeLessThanOrEqual(6);
    },
  );

  specTest(
    'uses the same jittered delay for the deadline check and retry sleep',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'the-deadline-check-and-the-retry-sleep-use-the-same-jittered-delay',
    },
    async () => {
      const realRandom = Math.random;
      const realNow = Date.now;
      let randomCalls = 0;
      Math.random = () => (randomCalls++ === 0 ? 0 : 1);
      // Freeze the clock so the overall-deadline check is load-independent. The
      // jittered delay (baseDelayMs 80 × 0.5 = 40ms) must fit inside maxElapsedMs
      // (60ms) for the retry to proceed; with the real clock, scheduler latency
      // under a full `--all` run can burn the ~20ms of slack before the check and
      // spuriously skip the retry (surfacing the 503). The behaviour under test —
      // one `computeDelay` call whose delay feeds both the deadline gate and the
      // sleep — is asserted by `randomCalls === 1` and is clock-independent.
      Date.now = () => 1000;

      try {
        const interceptor = retryInterceptor(
          { maxRetries: 1, baseDelayMs: 80, maxDelayMs: 80, retryableStatuses: [503], jitter: true },
          { timeoutMs: 1000, maxElapsedMs: 60 },
        );
        let calls = 0;

        const result = await interceptor(makeRequest(), async () => {
          calls++;
          return calls === 1 ? makeResponse(503) : makeResponse(200);
        });

        expect(result.status).toBe(200);
        expect(calls).toBe(2);
        expect(randomCalls).toBe(1);
      } finally {
        Math.random = realRandom;
        Date.now = realNow;
      }
    },
  );

  specTest(
    'surfaces ClientRetryExhaustedError when the deadline ends a network-error sequence',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-deadline-ending-a-network-error-sequence-surfaces-retry-exhausted',
    },
    async () => {
      const interceptor = retryInterceptor(
        { maxRetries: 10, baseDelayMs: 5, jitter: false },
        { timeoutMs: 30, maxElapsedMs: 30, getServiceName: () => 'orders-service' },
      );

      let calls = 0;
      const next = (_req: ClientRequest): Promise<ClientResponse> => {
        calls++;
        return Promise.reject(new TypeError('fetch failed: connection refused'));
      };

      const error = await interceptor(makeRequest(), next).catch((e) => e);
      expect(error).toBeInstanceOf(ClientRetryExhaustedError);
      expect((error as ClientRetryExhaustedError).service).toBe('orders-service');
      // Stopped by the deadline well before exhausting 10 retries.
      expect(calls).toBeLessThan(11);
    },
  );

  specTest(
    'surfaces ClientRetryExhaustedError when a backoff sleep overshoots the deadline',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-backoff-sleep-overshooting-the-deadline-surfaces-retry-exhausted',
    },
    async () => {
      // The deadline can end a network-error sequence two ways: the backoff being
      // skipped because it *would* overrun (covered above), or a scheduled backoff
      // *overshooting* its target so the next attempt's budget check finds the
      // deadline already passed. The latter races on the scheduler and only shows
      // up under load, so drive it deterministically with a stepped clock: each
      // read advances 10ms, so the first attempt is allowed to back off (25 < 30)
      // and the second iteration's budget check reads the clock already at the
      // deadline — exercising the top-of-loop break rather than the skip path.
      const realNow = Date.now;
      let tick = 0;
      Date.now = () => 10 * tick++;
      try {
        const interceptor = retryInterceptor(
          { maxRetries: 10, baseDelayMs: 5, jitter: false },
          { timeoutMs: 1000, maxElapsedMs: 30, getServiceName: () => 'orders-service' },
        );

        let calls = 0;
        const next = (_req: ClientRequest): Promise<ClientResponse> => {
          calls++;
          return Promise.reject(new TypeError('fetch failed: connection refused'));
        };

        const error = await interceptor(makeRequest(), next).catch((e) => e);
        expect(error).toBeInstanceOf(ClientRetryExhaustedError);
        expect((error as ClientRetryExhaustedError).service).toBe('orders-service');
        // The deadline break stopped the sequence before a second attempt ran; the
        // skip path would instead have rejected inside a second call (calls === 2).
        expect(calls).toBe(1);
      } finally {
        Date.now = realNow;
      }
    },
  );

  specTest(
    'no deadline is applied when timeoutMs is unset',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'no-deadline-applies-when-no-timeout-is-configured',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1, jitter: false });
      let calls = 0;

      const result = await interceptor(makeRequest(), async () => {
        calls++;
        if (calls <= 2) throw new TypeError('fetch failed');
        return makeResponse(200);
      });

      expect(result.status).toBe(200);
      expect(calls).toBe(3); // all retries available, no budget clamp
    },
  );
});

describe('retryInterceptor maxRetries clamping', () => {
  specTest(
    'clamps absurd maxRetries to the cap',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-cap',
      check: 'an-absurd-retry-count-is-clamped-to-the-cap',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 1_000_000, baseDelayMs: 0, retryableStatuses: [503] });
      let calls = 0;

      const result = await interceptor(makeRequest(), async () => {
        calls++;
        return makeResponse(503);
      });

      expect(result.status).toBe(503);
      // 1 initial + MAX_RETRIES_CAP retries, not a million.
      expect(calls).toBe(MAX_RETRIES_CAP + 1);
    },
  );

  specTest(
    'negative maxRetries is floored to zero (single attempt)',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-cap',
      check: 'a-negative-retry-count-means-no-retries',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: -5, retryableStatuses: [503] });
      let calls = 0;

      const result = await interceptor(makeRequest(), async () => {
        calls++;
        return makeResponse(503);
      });

      expect(result.status).toBe(503);
      expect(calls).toBe(1);
    },
  );

  specTest(
    'non-finite maxRetries is floored to zero (single attempt)',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-cap',
      check: 'a-non-finite-retry-count-means-no-retries',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: Number.NaN, retryableStatuses: [503] });
      let calls = 0;

      const result = await interceptor(makeRequest(), async () => {
        calls++;
        return makeResponse(503);
      });

      expect(result.status).toBe(503);
      expect(calls).toBe(1);
    },
  );
});
