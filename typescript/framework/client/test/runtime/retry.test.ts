import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { ClientRetryExhaustedError } from '../../src/runtime/errors';
import { computeDelay, retryInterceptor } from '../../src/runtime/retry';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

describe('computeDelay', () => {
  test('exponential backoff without jitter', () => {
    const config = { baseDelayMs: 100, maxDelayMs: 5000, maxRetries: 3, retryableStatuses: [], jitter: false };

    expect(computeDelay(0, config)).toBe(100);
    expect(computeDelay(1, config)).toBe(200);
    expect(computeDelay(2, config)).toBe(400);
    expect(computeDelay(3, config)).toBe(800);
  });

  specTest(
    'caps at maxDelayMs',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'backoff-is-capped-at-the-maximum-delay',
    },
    () => {
      const config = { baseDelayMs: 1000, maxDelayMs: 2000, maxRetries: 3, retryableStatuses: [], jitter: false };

      expect(computeDelay(0, config)).toBe(1000);
      expect(computeDelay(1, config)).toBe(2000);
      expect(computeDelay(2, config)).toBe(2000); // Capped
      expect(computeDelay(5, config)).toBe(2000); // Capped
    },
  );

  test('jitter produces values in expected range', () => {
    const config = { baseDelayMs: 100, maxDelayMs: 5000, maxRetries: 3, retryableStatuses: [], jitter: true };

    for (let i = 0; i < 20; i++) {
      const delay = computeDelay(0, config);
      // With jitter: capped * (0.5 to 1.0), so 50-100 for attempt 0
      expect(delay).toBeGreaterThanOrEqual(50);
      expect(delay).toBeLessThanOrEqual(100);
    }
  });
});

describe('retryInterceptor', () => {
  function makeRequest(): ClientRequest {
    return { method: 'GET', path: '/test', headers: new Headers() };
  }

  function makeResponse(status: number): ClientResponse {
    return { data: { ok: true }, status, headers: new Headers() };
  }

  test('passes through successful responses', async () => {
    const interceptor = retryInterceptor({ maxRetries: 3 });
    let callCount = 0;

    const result = await interceptor(makeRequest(), async () => {
      callCount++;
      return makeResponse(200);
    });

    expect(result.status).toBe(200);
    expect(callCount).toBe(1);
  });

  specTest(
    'retries on retryable status codes',
    { feature: 'typescript/service-clients', requirement: 'retry-scope', check: 'a-retryable-status-is-retried' },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 2,
        baseDelayMs: 1,
        retryableStatuses: [503],
      });
      let callCount = 0;

      const result = await interceptor(makeRequest(), async () => {
        callCount++;
        if (callCount < 3) return makeResponse(503);
        return makeResponse(200);
      });

      expect(result.status).toBe(200);
      expect(callCount).toBe(3); // 1 initial + 2 retries
    },
  );

  test('returns last response when retries exhausted', async () => {
    const interceptor = retryInterceptor({
      maxRetries: 2,
      baseDelayMs: 1,
      retryableStatuses: [503],
    });
    let callCount = 0;

    const result = await interceptor(makeRequest(), async () => {
      callCount++;
      return makeResponse(503);
    });

    expect(result.status).toBe(503);
    expect(callCount).toBe(3); // 1 initial + 2 retries
  });

  specTest(
    'does not retry non-retryable status codes',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'a-non-retryable-status-is-not-retried',
    },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 3,
        baseDelayMs: 1,
        retryableStatuses: [503],
      });
      let callCount = 0;

      const result = await interceptor(makeRequest(), async () => {
        callCount++;
        return makeResponse(400);
      });

      expect(result.status).toBe(400);
      expect(callCount).toBe(1);
    },
  );

  specTest(
    'retries on network errors (TypeError)',
    { feature: 'typescript/service-clients', requirement: 'retry-scope', check: 'a-network-error-is-retried' },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 2,
        baseDelayMs: 1,
      });
      let callCount = 0;

      const result = await interceptor(makeRequest(), async () => {
        callCount++;
        if (callCount < 3) throw new TypeError('fetch failed');
        return makeResponse(200);
      });

      expect(result.status).toBe(200);
      expect(callCount).toBe(3);
    },
  );

  specTest(
    'does not retry on abort',
    { feature: 'typescript/service-clients', requirement: 'cancellation', check: 'an-aborted-attempt-is-not-retried' },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });
      const controller = new AbortController();
      controller.abort();

      const request = { ...makeRequest(), signal: controller.signal };

      await expect(
        interceptor(request, async () => {
          throw new DOMException('Aborted', 'AbortError');
        }),
      ).rejects.toThrow('Aborted');
    },
  );

  test('does not retry non-network errors', async () => {
    const interceptor = retryInterceptor({
      maxRetries: 2,
      baseDelayMs: 1,
    });
    let callCount = 0;

    await expect(
      interceptor(makeRequest(), async () => {
        callCount++;
        throw new Error('boom');
      }),
    ).rejects.toThrow('boom');

    expect(callCount).toBe(1);
  });

  test('throws ClientRetryExhaustedError after network-error retries are exhausted', async () => {
    const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1 }, () => 'users-service');
    let callCount = 0;

    const promise = interceptor(makeRequest(), async () => {
      callCount++;
      throw new TypeError('connection refused');
    });

    await expect(promise).rejects.toBeInstanceOf(ClientRetryExhaustedError);
    expect(callCount).toBe(3); // 1 initial + 2 retries

    const error = await promise.catch((e: unknown) => e as ClientRetryExhaustedError);
    expect(error.attempts).toBe(3);
    expect(error.lastError).toBeInstanceOf(TypeError);
    expect(error.service).toBe('users-service');
    expect(error.method).toBe('GET /test');
  });

  specTest(
    'rejects immediately when the signal is already aborted before a retry delay',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'an-already-aborted-signal-rejects-before-a-retry-delay',
    },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 1,
        baseDelayMs: 10,
        retryableStatuses: [503],
        jitter: false,
      });
      const controller = new AbortController();
      const reason = new Error('cancelled-before-sleep');
      controller.abort(reason);

      await expect(
        interceptor({ ...makeRequest(), signal: controller.signal }, async () => makeResponse(503)),
      ).rejects.toBe(reason);
    },
  );

  specTest(
    'rejects when the signal aborts during the retry delay',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'an-abort-during-the-retry-delay-ends-the-sequence',
    },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 1,
        baseDelayMs: 25,
        retryableStatuses: [503],
        jitter: false,
      });
      const controller = new AbortController();
      const reason = new Error('cancelled-during-sleep');

      const pending = interceptor({ ...makeRequest(), signal: controller.signal }, async () => makeResponse(503));
      setTimeout(() => controller.abort(reason), 5);

      await expect(pending).rejects.toBe(reason);
    },
  );
});

describe('retryInterceptor telemetry', () => {
  function makeRequest(): ClientRequest {
    return { method: 'GET', path: '/test', headers: new Headers() };
  }

  function makeResponse(status: number): ClientResponse {
    return { data: { ok: true }, status, headers: new Headers() };
  }

  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    setCollector(undefined);
  });

  /**
   * Drain the collector once and merge all counters across buckets. `drainAll`
   * is destructive, so a test that inspects several counter names must drain a
   * single time and read from the merged snapshot.
   */
  function drainCounters(): Record<string, number> {
    const merged: Record<string, number> = {};
    for (const bucket of collector.drainAll()) {
      for (const [name, value] of Object.entries(bucket.counters)) {
        merged[name] = (merged[name] ?? 0) + value;
      }
    }
    return merged;
  }

  specTest(
    'counts one attempt per retried attempt (retryable status)',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'one-attempt-counter-per-retried-attempt',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1, retryableStatuses: [503] }, () => 'users');
      let callCount = 0;

      await interceptor(makeRequest(), async () => {
        callCount++;
        return makeResponse(503); // always retryable → exhausts retries
      });

      expect(callCount).toBe(3); // 1 initial + 2 retries
      const counters = drainCounters();
      expect(counters['client.users.retry.attempt']).toBe(2); // one per retry, not per attempt
      // Gave up while the response was still retryable → exhausted, not succeeded.
      expect(counters['client.users.retry.exhausted']).toBe(1);
      expect(counters['client.users.retry.succeeded'] ?? 0).toBe(0);
    },
  );

  specTest(
    'counts attempts on network errors',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'network-error-attempts-are-counted',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1 }, () => 'orders');
      let callCount = 0;

      await interceptor(makeRequest(), async () => {
        callCount++;
        if (callCount < 3) throw new TypeError('fetch failed');
        return makeResponse(200);
      }).catch(() => {});

      const counters = drainCounters();
      expect(counters['client.orders.retry.attempt']).toBe(2);
      // Recovered on the third attempt → succeeded-after-retry.
      expect(counters['client.orders.retry.succeeded']).toBe(1);
      expect(counters['client.orders.retry.exhausted'] ?? 0).toBe(0);
    },
  );

  specTest(
    'counts retries-exhausted when network errors give up',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'retry-exhaustion-is-counted',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1 }, () => 'carts');

      await interceptor(makeRequest(), async () => {
        throw new TypeError('fetch failed'); // never recovers → exhausts retries
      }).catch(() => {});

      const counters = drainCounters();
      expect(counters['client.carts.retry.attempt']).toBe(2);
      expect(counters['client.carts.retry.exhausted']).toBe(1);
      expect(counters['client.carts.retry.succeeded'] ?? 0).toBe(0);
    },
  );

  specTest(
    'emits no retry counters when the first attempt succeeds',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'no-retry-counter-is-emitted-when-the-first-attempt-succeeds',
    },
    async () => {
      const interceptor = retryInterceptor(
        { maxRetries: 3, baseDelayMs: 1, retryableStatuses: [503] },
        () => 'widgets',
      );

      await interceptor(makeRequest(), async () => makeResponse(200));

      const counters = drainCounters();
      expect(counters['client.widgets.retry.attempt'] ?? 0).toBe(0);
      expect(counters['client.widgets.retry.succeeded'] ?? 0).toBe(0);
      expect(counters['client.widgets.retry.exhausted'] ?? 0).toBe(0);
    },
  );

  specTest(
    'falls back to client.retry.* when no service name is provided',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'retry-metrics-fall-back-to-an-unnamed-prefix',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 1, baseDelayMs: 1, retryableStatuses: [503] });

      await interceptor(makeRequest(), async () => makeResponse(503));

      const counters = drainCounters();
      expect(counters['client.retry.attempt']).toBe(1);
      expect(counters['client.retry.exhausted']).toBe(1);
    },
  );
});
