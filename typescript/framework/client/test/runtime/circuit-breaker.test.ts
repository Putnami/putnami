import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { CircuitBreaker, CircuitOpenError, circuitBreakerInterceptor } from '../../src/runtime/circuit-breaker';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';
import { isDisposableInterceptor } from '../../src/runtime/transport.type';

function makeRequest(): ClientRequest {
  return { method: 'GET', path: '/test', headers: new Headers() };
}

function makeResponse(status: number): ClientResponse {
  return { data: { ok: true }, status, headers: new Headers() };
}

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitFor(fn: () => boolean, timeoutMs = 500, intervalMs = 5): Promise<void> {
  const start = Date.now();
  while (!fn()) {
    if (Date.now() - start > timeoutMs) throw new Error('waitFor timed out');
    // biome-ignore lint/performance/noAwaitInLoops: bounded test polling is intentionally sequential
    await wait(intervalMs);
  }
}

describe('CircuitBreaker', () => {
  test('starts in closed state', () => {
    const breaker = new CircuitBreaker();
    expect(breaker.getState()).toBe('closed');
    expect(breaker.allowRequest()).toBe(true);
  });

  specTest(
    'opens after failure threshold',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'the-breaker-opens-after-the-configured-consecutive-failures',
    },
    () => {
      const breaker = new CircuitBreaker({ failureThreshold: 3 });

      breaker.onFailure();
      expect(breaker.getState()).toBe('closed');
      breaker.onFailure();
      expect(breaker.getState()).toBe('closed');
      breaker.onFailure();
      expect(breaker.getState()).toBe('open');
      expect(breaker.allowRequest()).toBe(false);

      breaker.dispose();
    },
  );

  specTest(
    'resets failure count on success',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'a-success-resets-the-failure-count',
    },
    () => {
      const breaker = new CircuitBreaker({ failureThreshold: 3 });

      breaker.onFailure();
      breaker.onFailure();
      breaker.onSuccess(); // Should reset count
      breaker.onFailure();
      breaker.onFailure();

      expect(breaker.getState()).toBe('closed'); // 2 failures, not 3

      breaker.dispose();
    },
  );

  specTest(
    'transitions to half-open after timeout',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'the-breaker-goes-half-open-after-the-timeout',
    },
    () => {
      const breaker = new CircuitBreaker({
        failureThreshold: 1,
        resetTimeoutMs: 50,
      });

      breaker.onFailure();
      expect(breaker.getState()).toBe('open');

      // Manually advance by waiting
      return new Promise<void>((resolve) => {
        setTimeout(() => {
          expect(breaker.allowRequest()).toBe(true);
          expect(breaker.getState()).toBe('half-open');
          breaker.dispose();
          resolve();
        }, 100);
      });
    },
  );

  specTest(
    'closes from half-open after success threshold',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'the-breaker-closes-from-half-open-after-the-success-threshold',
    },
    () => {
      const breaker = new CircuitBreaker({
        failureThreshold: 1,
        resetTimeoutMs: 0,
        successThreshold: 2,
      });

      breaker.onFailure();
      expect(breaker.getState()).toBe('open');

      // Force half-open by allowing request after timeout
      breaker.allowRequest();

      breaker.onSuccess();
      expect(breaker.getState()).toBe('half-open');
      breaker.onSuccess();
      expect(breaker.getState()).toBe('closed');

      breaker.dispose();
    },
  );

  specTest(
    'reopens from half-open on failure',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'the-breaker-reopens-from-half-open-on-a-failure',
    },
    () => {
      const breaker = new CircuitBreaker({
        failureThreshold: 1,
        resetTimeoutMs: 0,
        successThreshold: 2,
      });

      breaker.onFailure();
      breaker.allowRequest(); // Forces half-open

      breaker.onFailure(); // Should go back to open
      expect(breaker.getState()).toBe('open');

      breaker.dispose();
    },
  );

  specTest(
    'half-open admits only one trial by default, rejecting the rest',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'half-open-admits-one-trial-by-default-and-rejects-the-rest',
    },
    () => {
      const breaker = new CircuitBreaker({ failureThreshold: 1, resetTimeoutMs: 0, successThreshold: 5 });

      breaker.onFailure(); // open
      // First allowRequest transitions to half-open and admits one trial.
      expect(breaker.allowRequest()).toBe(true);
      expect(breaker.getState()).toBe('half-open');
      // Concurrent trials (no completion yet) are rejected.
      expect(breaker.allowRequest()).toBe(false);
      expect(breaker.allowRequest()).toBe(false);

      breaker.dispose();
    },
  );

  specTest(
    'half-open admits another trial after the in-flight one completes',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'half-open-admits-another-trial-once-the-in-flight-one-completes',
    },
    () => {
      const breaker = new CircuitBreaker({ failureThreshold: 1, resetTimeoutMs: 0, successThreshold: 5 });

      breaker.onFailure(); // open
      expect(breaker.allowRequest()).toBe(true); // trial 1 admitted (in-flight)
      expect(breaker.allowRequest()).toBe(false); // gated

      breaker.onSuccess(); // trial 1 completes; still half-open (successThreshold 5)
      expect(breaker.getState()).toBe('half-open');
      expect(breaker.allowRequest()).toBe(true); // trial 2 now admitted

      breaker.dispose();
    },
  );

  test('an ignored local failure releases a half-open slot without healing the service', () => {
    const breaker = new CircuitBreaker({ failureThreshold: 1, resetTimeoutMs: 0 });
    breaker.onFailure();
    expect(breaker.allowRequest()).toBe(true);

    breaker.onIgnored();

    expect(breaker.getState()).toBe('half-open');
    expect(breaker.allowRequest()).toBe(true);
    breaker.dispose();
  });

  test('an ignored local failure does not erase failures recorded while closed', () => {
    const breaker = new CircuitBreaker({ failureThreshold: 2 });
    breaker.onFailure();

    breaker.onIgnored();
    breaker.onFailure();

    expect(breaker.getState()).toBe('open');
    breaker.dispose();
  });

  specTest(
    'honors a custom halfOpenMaxConcurrent',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'a-custom-half-open-concurrency-is-honoured',
    },
    () => {
      const breaker = new CircuitBreaker({
        failureThreshold: 1,
        resetTimeoutMs: 0,
        successThreshold: 5,
        halfOpenMaxConcurrent: 2,
      });

      breaker.onFailure(); // open
      expect(breaker.allowRequest()).toBe(true); // trial 1
      expect(breaker.allowRequest()).toBe(true); // trial 2
      expect(breaker.allowRequest()).toBe(false); // gated at 2

      breaker.dispose();
    },
  );

  test('health probing transitions to half-open and stops once the circuit is no longer open', async () => {
    const server = Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === '/health') {
          return Response.json({ ok: true });
        }
        return new Response('Not found', { status: 404 });
      },
    });

    const breaker = new CircuitBreaker({
      failureThreshold: 1,
      healthCheckUrl: `http://localhost:${server.port}/health`,
      healthCheckIntervalMs: 5,
    });

    try {
      breaker.onFailure();
      await waitFor(() => breaker.getState() === 'half-open');
      expect(breaker.getState()).toBe('half-open');

      await waitFor(
        () =>
          (breaker as unknown as { healthCheckTimer?: ReturnType<typeof setInterval> }).healthCheckTimer === undefined,
      );
    } finally {
      breaker.dispose();
      server.stop(true);
    }
  });

  test('health probing tolerates probe failures', async () => {
    const breaker = new CircuitBreaker({
      failureThreshold: 1,
      healthCheckUrl: 'http://127.0.0.1:1/health',
      healthCheckIntervalMs: 5,
    });

    try {
      breaker.onFailure();
      await wait(20);
      expect(breaker.getState()).toBe('open');
    } finally {
      breaker.dispose();
    }
  });
});

describe('circuitBreakerInterceptor', () => {
  test('passes through when circuit is closed', async () => {
    const interceptor = circuitBreakerInterceptor({ failureThreshold: 5 });

    const response = await interceptor(makeRequest(), async () => makeResponse(200));
    expect(response.status).toBe(200);
  });

  test('records failures and opens circuit', async () => {
    const interceptor = circuitBreakerInterceptor({
      failureThreshold: 2,
      failureStatuses: [500],
    });

    // Two failures
    await interceptor(makeRequest(), async () => makeResponse(500));
    await interceptor(makeRequest(), async () => makeResponse(500));

    // Circuit should now be open
    try {
      await interceptor(makeRequest(), async () => makeResponse(200));
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(CircuitOpenError);
      expect((error as CircuitOpenError).circuitState).toBe('open');
    }
  });

  test('records network errors as failures', async () => {
    const interceptor = circuitBreakerInterceptor({
      failureThreshold: 1,
    });

    try {
      await interceptor(makeRequest(), async () => {
        throw new TypeError('fetch failed');
      });
    } catch {
      // Expected
    }

    // Circuit should now be open
    try {
      await interceptor(makeRequest(), async () => makeResponse(200));
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(CircuitOpenError);
    }
  });

  specTest(
    'does not count 4xx as failures',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'a-4xx-does-not-count-as-a-breaker-failure',
    },
    async () => {
      const interceptor = circuitBreakerInterceptor({
        failureThreshold: 1,
        failureStatuses: [500, 502, 503],
      });

      await interceptor(makeRequest(), async () => makeResponse(400));
      await interceptor(makeRequest(), async () => makeResponse(404));
      await interceptor(makeRequest(), async () => makeResponse(422));

      // Should still be allowed (4xx are not failures)
      const response = await interceptor(makeRequest(), async () => makeResponse(200));
      expect(response.status).toBe(200);
    },
  );

  specTest(
    'rejects concurrent trial requests in half-open state',
    {
      feature: 'typescript/service-clients',
      requirement: 'circuit-isolation',
      check: 'a-concurrent-trial-request-is-rejected-while-half-open',
    },
    async () => {
      const interceptor = circuitBreakerInterceptor({
        failureThreshold: 1,
        resetTimeoutMs: 0, // immediately eligible for half-open
        successThreshold: 5, // stay half-open across several successes
        failureStatuses: [500],
      });

      // Trip the breaker open.
      await interceptor(makeRequest(), async () => makeResponse(500));

      // A slow trial request occupies the single half-open slot.
      let releaseTrial!: () => void;
      const trialGate = new Promise<void>((resolve) => {
        releaseTrial = resolve;
      });
      const trial = interceptor(makeRequest(), async () => {
        await trialGate;
        return makeResponse(200);
      });

      // Give the trial a tick to enter the breaker and occupy the slot.
      await wait(5);

      // Concurrent requests while the trial is in flight must be rejected.
      await expect(interceptor(makeRequest(), async () => makeResponse(200))).rejects.toBeInstanceOf(CircuitOpenError);
      await expect(interceptor(makeRequest(), async () => makeResponse(200))).rejects.toBeInstanceOf(CircuitOpenError);

      // Let the trial finish; the slot frees up afterwards.
      releaseTrial();
      await trial;
    },
  );
});

describe('CircuitBreaker telemetry', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    setCollector(undefined);
  });

  test('normalizes dotted contract identities for circuit metric keys', () => {
    const breaker = new CircuitBreaker({ serviceName: 'catalog.items' });
    expect(breaker.getState()).toBe('closed');
    breaker.dispose();
  });

  specTest(
    'emits a state gauge and per-transition counters on each transition',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'each-circuit-transition-emits-a-counter-and-a-state-gauge',
    },
    () => {
      const breaker = new CircuitBreaker({
        serviceName: 'users',
        failureThreshold: 1,
        resetTimeoutMs: 0,
        successThreshold: 1,
      });

      breaker.onFailure(); // closed → open
      breaker.allowRequest(); // open → half-open (resetTimeoutMs 0)
      breaker.onSuccess(); // half-open → closed (successThreshold 1)
      breaker.dispose();

      const bucket = collector.drainAll()[0];
      // Gauge is last-write-wins within the second → final state is closed (0).
      expect(bucket.gauges['client.users.circuit.state']).toBe(0);
      // One counter per transition we drove.
      expect(bucket.counters['client.users.circuit.transition.open']).toBe(1);
      expect(bucket.counters['client.users.circuit.transition.half-open']).toBe(1);
      expect(bucket.counters['client.users.circuit.transition.closed']).toBe(1);
    },
  );

  specTest(
    'open transition sets the state gauge to 1',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'an-open-transition-sets-the-state-gauge',
    },
    () => {
      const breaker = new CircuitBreaker({ serviceName: 'orders', failureThreshold: 1 });

      breaker.onFailure(); // closed → open
      breaker.dispose();

      const bucket = collector.drainAll()[0];
      expect(bucket.gauges['client.orders.circuit.state']).toBe(1);
      expect(bucket.counters['client.orders.circuit.transition.open']).toBe(1);
    },
  );

  specTest(
    'falls back to client.circuit.* when no service name is provided',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'circuit-metrics-fall-back-to-an-unnamed-prefix',
    },
    () => {
      const breaker = new CircuitBreaker({ failureThreshold: 1 });

      breaker.onFailure(); // closed → open
      breaker.dispose();

      const bucket = collector.drainAll()[0];
      expect(bucket.gauges['client.circuit.state']).toBe(1);
      expect(bucket.counters['client.circuit.transition.open']).toBe(1);
    },
  );
});

describe('circuitBreakerInterceptor disposal', () => {
  test('returns a disposable interceptor', () => {
    const interceptor = circuitBreakerInterceptor({ failureThreshold: 1 });
    expect(isDisposableInterceptor(interceptor)).toBe(true);
    expect(typeof interceptor.dispose).toBe('function');
    interceptor.dispose();
  });

  specTest(
    'dispose() stops the health probe so no further probes are issued',
    {
      feature: 'typescript/service-clients',
      requirement: 'resource-release',
      check: 'disposing-the-breaker-stops-its-health-probe',
    },
    async () => {
      let probeCount = 0;
      const server = Bun.serve({
        port: 0,
        fetch() {
          probeCount++;
          return new Response('unavailable', { status: 503 }); // never recovers → stays open, keeps probing
        },
      });

      const interceptor = circuitBreakerInterceptor({
        failureThreshold: 1,
        failureStatuses: [503],
        healthCheckUrl: `http://localhost:${server.port}/health`,
        healthCheckIntervalMs: 5,
      });

      try {
        // Trip the breaker open → starts the health-probe interval.
        await interceptor(makeRequest(), async () => makeResponse(503));

        // Let at least one probe fire.
        await waitFor(() => probeCount >= 1);

        // Dispose stops the interval. A probe fetch already in flight may still
        // land, so settle briefly before snapshotting the baseline.
        interceptor.dispose();
        await wait(20);
        const countAfterDispose = probeCount;

        // Well beyond several probe intervals (5ms), no NEW probes are issued.
        await wait(40);
        expect(probeCount).toBe(countAfterDispose);
      } finally {
        interceptor.dispose();
        server.stop(true);
      }
    },
  );
});
