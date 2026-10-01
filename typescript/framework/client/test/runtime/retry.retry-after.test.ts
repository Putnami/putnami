import { describe, expect, jest, test } from 'bun:test';
import type { ClientContractOperation } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { ClientFrameworkError } from '../../src/runtime/errors';
import { parseRetryAfter, retryInterceptor } from '../../src/runtime/retry';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function operation(
  errors: ClientContractOperation['errors'],
  resilience?: ClientContractOperation['resilience'],
): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/items', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors,
    idempotency: { kind: 'idempotent' },
    ...(resilience ? { resilience } : {}),
  };
}

function request(clientOperation?: ClientContractOperation): ClientRequest {
  return {
    method: 'GET',
    path: '/items',
    headers: new Headers(),
    operationId: 'listItems',
    ...(clientOperation ? { clientOperation } : {}),
  };
}

function response(status: number, retryAfter?: string): ClientResponse {
  const headers = new Headers();
  if (retryAfter !== undefined) headers.set('retry-after', retryAfter);
  return { data: undefined, status, headers };
}

/** Let every already-scheduled continuation run without advancing the clock. */
async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 20; i++) await Promise.resolve();
}

describe('parseRetryAfter', () => {
  test('reads both forms RFC 9110 defines and refuses anything else', () => {
    const now = Date.UTC(2026, 8, 9, 12, 0, 0);
    expect(parseRetryAfter('7', now)).toBe(7000);
    expect(parseRetryAfter('0', now)).toBe(0);
    expect(parseRetryAfter('  12  ', now)).toBe(12_000);
    expect(parseRetryAfter('Wed, 09 Sep 2026 12:00:30 GMT', now)).toBe(30_000);
    // A date already past asks for an immediate retry, not a negative wait.
    expect(parseRetryAfter('Wed, 09 Sep 2026 11:59:00 GMT', now)).toBe(0);
    for (const value of [null, undefined, '', '   ', '-3', '3.5', 'soon', '1e3']) {
      expect(parseRetryAfter(value, now), String(value)).toBeUndefined();
    }
  });
});

describe('retryInterceptor and the provider Retry-After budget', () => {
  specTest(
    'waits exactly as long as the provider asked instead of its own backoff',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-provider-retry-after-budget-replaces-the-computed-backoff',
    },
    async () => {
      jest.useFakeTimers();
      try {
        // The computed backoff would be 1ms. The provider asks for 4s; the client
        // must still be waiting after 3999ms and must resume at 4000ms.
        const interceptor = retryInterceptor(
          { maxRetries: 1, baseDelayMs: 1, jitter: false, retryableStatuses: [503] },
          { timeoutMs: 60_000, maxElapsedMs: 600_000 },
        );
        let attempts = 0;
        const call = interceptor(request(), (attempt) => {
          attempts++;
          if (attempt.retryState) attempt.retryState.retryAfterMs = parseRetryAfter('4');
          return Promise.resolve(attempts === 1 ? response(503, '4') : response(200));
        });
        let settled = false;
        void call.then(() => {
          settled = true;
        });

        // Let the first attempt run and schedule its wait before moving the clock.
        await flushMicrotasks();
        expect(attempts).toBe(1);
        jest.advanceTimersByTime(3999);
        await flushMicrotasks();
        expect(settled).toBe(false);

        jest.advanceTimersByTime(1);
        await flushMicrotasks();
        expect((await call).status).toBe(200);
        expect(attempts).toBe(2);
      } finally {
        jest.useRealTimers();
      }
    },
  );

  specTest(
    'gives up rather than retry earlier than the provider asked',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-retry-after-budget-larger-than-the-remaining-deadline-ends-the-sequence',
    },
    async () => {
      // 30s asked for, 200ms of budget left: shortening the wait is exactly what
      // the header exists to prevent, so the sequence ends with the last response.
      const interceptor = retryInterceptor(
        { maxRetries: 3, baseDelayMs: 1, jitter: false, retryableStatuses: [503] },
        { timeoutMs: 200, maxElapsedMs: 200 },
      );
      let attempts = 0;
      const started = Date.now();
      const result = await interceptor(request(), (attempt) => {
        attempts++;
        if (attempt.retryState) attempt.retryState.retryAfterMs = parseRetryAfter('30');
        return Promise.resolve(response(503, '30'));
      });
      expect(result.status).toBe(503);
      expect(attempts).toBe(1);
      // No sleep was taken: the deadline decided before any wait started.
      expect(Date.now() - started).toBeLessThan(200);
    },
  );

  specTest(
    'never retries a status the operation declares non-retryable, Retry-After included',
    {
      feature: 'typescript/service-clients',
      requirement: 'bounded-latency',
      check: 'a-declared-non-retryable-status-outranks-the-generic-retryable-set',
    },
    async () => {
      const declared = operation([{ status: 503, code: 'http.service_unavailable', retryable: false }]);
      const interceptor = retryInterceptor(
        { maxRetries: 3, baseDelayMs: 1, jitter: false, retryableStatuses: [503] },
        { timeoutMs: 60_000, maxElapsedMs: 60_000 },
      );

      let attempts = 0;
      const result = await interceptor(request(declared), (attempt) => {
        attempts++;
        if (attempt.retryState) attempt.retryState.retryAfterMs = parseRetryAfter('0');
        return Promise.resolve(response(503, '0'));
      });
      expect(result.status).toBe(503);
      expect(attempts).toBe(1);

      // The same declaration also outranks the header on the throwing path.
      let thrownAttempts = 0;
      const failure = await interceptor(request(declared), (attempt) => {
        thrownAttempts++;
        if (attempt.retryState) attempt.retryState.retryAfterMs = parseRetryAfter('0');
        return Promise.reject(
          new ClientFrameworkError({
            service: 'items',
            method: 'listItems',
            status: 503,
            code: 'http.service_unavailable',
          }),
        );
      }).catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(ClientFrameworkError);
      expect(thrownAttempts).toBe(1);
    },
  );
});
