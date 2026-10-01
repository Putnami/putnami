import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { computeDelay, retryInterceptor } from '../../src/runtime/retry';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function makeRequest(signal?: AbortSignal): ClientRequest {
  return { method: 'GET', path: '/test', headers: new Headers(), signal };
}

function makeResponse(status: number): ClientResponse {
  return { data: { ok: true }, status, headers: new Headers() };
}

// ---------------------------------------------------------------------------
// computeDelay edge cases
// ---------------------------------------------------------------------------

describe('computeDelay edge cases', () => {
  test('attempt 0 returns base delay without jitter', () => {
    const config = { baseDelayMs: 500, maxDelayMs: 10_000, maxRetries: 3, retryableStatuses: [], jitter: false };
    expect(computeDelay(0, config)).toBe(500);
  });

  test('very high attempt number is still capped at maxDelayMs', () => {
    const config = { baseDelayMs: 100, maxDelayMs: 1000, maxRetries: 10, retryableStatuses: [], jitter: false };
    expect(computeDelay(100, config)).toBe(1000);
  });

  test('jitter reduces delay but stays above 50% of capped value', () => {
    const config = { baseDelayMs: 200, maxDelayMs: 10_000, maxRetries: 3, retryableStatuses: [], jitter: true };

    for (let i = 0; i < 50; i++) {
      const delay = computeDelay(1, config);
      // attempt 1: baseDelay * 2^1 = 400, jitter: 200-400
      expect(delay).toBeGreaterThanOrEqual(200);
      expect(delay).toBeLessThanOrEqual(400);
    }
  });

  test('zero base delay always returns 0', () => {
    const config = { baseDelayMs: 0, maxDelayMs: 1000, maxRetries: 3, retryableStatuses: [], jitter: false };
    expect(computeDelay(0, config)).toBe(0);
    expect(computeDelay(5, config)).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// retryInterceptor edge cases
// ---------------------------------------------------------------------------

describe('retryInterceptor edge cases', () => {
  specTest(
    'maxRetries 0 means no retries',
    { feature: 'typescript/service-clients', requirement: 'retry-cap', check: 'a-zero-retry-count-means-no-retries' },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 0, retryableStatuses: [503] });
      let callCount = 0;

      const result = await interceptor(makeRequest(), async () => {
        callCount++;
        return makeResponse(503);
      });

      expect(result.status).toBe(503);
      expect(callCount).toBe(1);
    },
  );

  specTest(
    'retries all 4 default retryable statuses',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'the-four-default-retryable-statuses-are-retried',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 1, baseDelayMs: 1 });

      for (const status of [429, 502, 503, 504]) {
        let callCount = 0;
        // biome-ignore lint/performance/noAwaitInLoops: each status owns isolated retry state asserted before the next case
        await interceptor(makeRequest(), async () => {
          callCount++;
          if (callCount === 1) return makeResponse(status);
          return makeResponse(200);
        });
        expect(callCount).toBe(2);
      }
    },
  );

  specTest(
    'does not retry 400, 401, 403, 404, 500',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'client-error-and-500-statuses-are-not-retried',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });

      for (const status of [400, 401, 403, 404, 500]) {
        let callCount = 0;
        // biome-ignore lint/performance/noAwaitInLoops: each status owns isolated retry state asserted before the next case
        const result = await interceptor(makeRequest(), async () => {
          callCount++;
          return makeResponse(status);
        });
        expect(callCount).toBe(1);
        expect(result.status).toBe(status);
      }
    },
  );

  test('does not retry TimeoutError', async () => {
    const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });
    let callCount = 0;

    await expect(
      interceptor(makeRequest(), async () => {
        callCount++;
        const error = new DOMException('Signal timed out', 'TimeoutError');
        throw error;
      }),
    ).rejects.toThrow();

    expect(callCount).toBe(1);
  });

  test('retries TypeError (DNS/connection failures) up to maxRetries', async () => {
    const interceptor = retryInterceptor({ maxRetries: 2, baseDelayMs: 1 });
    let callCount = 0;

    await expect(
      interceptor(makeRequest(), async () => {
        callCount++;
        throw new TypeError('fetch failed: connection refused');
      }),
    ).rejects.toThrow('fetch failed');

    expect(callCount).toBe(3); // 1 initial + 2 retries
  });

  test('successful retry after transient failure', async () => {
    const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1, retryableStatuses: [503] });
    let callCount = 0;

    const result = await interceptor(makeRequest(), async () => {
      callCount++;
      if (callCount <= 2) return makeResponse(503);
      return makeResponse(200);
    });

    expect(result.status).toBe(200);
    expect(callCount).toBe(3);
  });

  test('successful retry after network error', async () => {
    const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });
    let callCount = 0;

    const result = await interceptor(makeRequest(), async () => {
      callCount++;
      if (callCount === 1) throw new TypeError('fetch failed');
      return makeResponse(200);
    });

    expect(result.status).toBe(200);
    expect(callCount).toBe(2);
  });

  specTest(
    'abort during first attempt stops immediately',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'an-abort-during-the-first-attempt-stops-immediately',
    },
    async () => {
      const controller = new AbortController();
      const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });

      controller.abort(new Error('user-cancelled'));

      await expect(
        interceptor(makeRequest(controller.signal), async () => {
          throw new DOMException('Aborted', 'AbortError');
        }),
      ).rejects.toThrow('Aborted');
    },
  );

  specTest(
    'does not retry non-Error throwables',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'a-non-error-throwable-is-not-retried',
    },
    async () => {
      const interceptor = retryInterceptor({ maxRetries: 3, baseDelayMs: 1 });
      let callCount = 0;

      await expect(
        interceptor(makeRequest(), async () => {
          callCount++;
          throw 'string-error'; // Not an Error instance
        }),
      ).rejects.toBe('string-error');

      expect(callCount).toBe(1);
    },
  );

  specTest(
    'custom retryableStatuses overrides defaults',
    {
      feature: 'typescript/service-clients',
      requirement: 'retry-scope',
      check: 'a-custom-retryable-status-list-overrides-the-defaults',
    },
    async () => {
      const interceptor = retryInterceptor({
        maxRetries: 1,
        baseDelayMs: 1,
        retryableStatuses: [418], // I'm a teapot
      });
      let callCount = 0;

      const result = await interceptor(makeRequest(), async () => {
        callCount++;
        return makeResponse(418);
      });

      expect(result.status).toBe(418);
      expect(callCount).toBe(2); // 1 initial + 1 retry
    },
  );
});
