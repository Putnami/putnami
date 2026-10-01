import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { CircuitBreaker } from '../../src/runtime/circuit-breaker';
import { CircuitOpenError } from '../../src/runtime/circuit-breaker';
import {
  ClientCanceledError,
  ClientCredentialError,
  ClientDeadlineError,
  ClientFrameworkError,
  ClientResponseContractError,
} from '../../src/runtime/errors';
import { StreamSession, type StreamBudgets, type StreamSessionOptions } from '../../src/runtime/stream-session';

const BUDGETS: StreamBudgets = {
  handshakeMs: 0,
  idleMs: 0,
  sessionMs: 0,
  heartbeatMs: 0,
  maxFrameBytes: 4096,
  maxBufferedMessages: 4,
};

function session<T>(overrides: Partial<StreamSessionOptions> = {}): StreamSession<T> {
  return new StreamSession<T>({
    serviceId: 'catalog.items',
    operationId: 'getWidgetsWatch',
    protocol: 'websocket',
    budgets: BUDGETS,
    ...overrides,
  });
}

describe('stream session phases', () => {
  specTest(
    'walks connecting to closed once, in order, and never goes back',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'a-session-reaches-closed-on-every-exit-path',
    },
    () => {
      const live = session();
      expect(live.phase).toBe('connecting');
      live.admit();
      expect(live.phase).toBe('admitted');
      live.deliver(undefined as never);
      expect(live.phase).toBe('active');
      live.complete();
      expect(live.phase).toBe('terminal');
      live.close();
      expect(live.phase).toBe('closed');
      live.admit();
      expect(live.phase).toBe('closed');
    },
  );

  test('delivers exactly one terminal to the caller, and the first one wins', () => {
    const live = session<number>();
    const errors: Error[] = [];
    let completions = 0;
    live.observer().onError((error) => errors.push(error));
    live.observer().onComplete(() => {
      completions += 1;
    });
    live.admit();
    const first = live.fail(new ClientResponseContractError('first'));
    const second = live.fail(new ClientResponseContractError('second'));
    live.complete();
    live.close();
    expect(errors).toHaveLength(1);
    expect(completions).toBe(0);
    expect(second).toBe(first);
    expect(live.error).toBe(first);
  });

  test('closing an undecided session records the cancel terminal first', () => {
    const live = session();
    const errors: Error[] = [];
    live.observer().onError((error) => errors.push(error));
    live.admit();
    live.close();
    expect(errors[0]).toBeInstanceOf(ClientCanceledError);
    expect(live.phase).toBe('closed');
  });

  specTest(
    'emits the single call measurement once, on every exit path',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'the-call-measurement-is-emitted-exactly-once',
    },
    () => {
      for (const exit of ['complete', 'fail', 'close'] as const) {
        const measurements: unknown[] = [];
        const live = session({ startCall: () => (result) => measurements.push(result) });
        live.admit();
        if (exit === 'complete') live.complete();
        if (exit === 'fail') live.fail(new ClientResponseContractError('broken'));
        live.close();
        live.close();
        expect({ exit, count: measurements.length }).toEqual({ exit, count: 1 });
      }
    },
  );

  test('the observer cancel path records the cancel terminal and one measurement carrying it', () => {
    const measurements: { error?: unknown }[] = [];
    const live = session({ startCall: () => (result) => measurements.push(result) });
    const errors: Error[] = [];
    // A caller who cancelled is not told why the stream ended — they decided it.
    // The session still records the terminal, so the measurement carries
    // `client.canceled` rather than an empty code.
    live.observer().onError((error) => errors.push(error));
    live.admit();
    live.observer().cancel();
    expect(errors).toEqual([]);
    expect(live.error).toBeInstanceOf(ClientCanceledError);
    expect(measurements).toHaveLength(1);
    expect(measurements[0]?.error).toBeInstanceOf(ClientCanceledError);
    expect(live.phase).toBe('closed');
  });
});

describe('the breaker rule is per phase', () => {
  const breaker = () => new CircuitBreaker({ failureThreshold: 1, resetTimeoutMs: 60_000 });

  specTest(
    'admission records one success, and a mid-stream break records nothing',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'a-mid-stream-break-writes-nothing-to-the-breaker',
    },
    () => {
      const circuit = breaker();
      const live = session({ breaker: circuit });
      live.dispatch();
      live.admit();
      live.admit();
      live.fail(new ClientResponseContractError('provider cut the stream'));
      live.close();
      expect(circuit.getState()).toBe('closed');
      expect(circuit.allowRequest()).toBe(true);
    },
  );

  specTest(
    'a refused handshake that reached the provider opens the circuit',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'a-refused-handshake-opens-the-circuit',
    },
    () => {
      const circuit = breaker();
      const live = session({ breaker: circuit });
      live.dispatch();
      live.fail(new ClientFrameworkError({ service: 's', method: 'm', status: 503, code: 'http.service_unavailable' }));
      live.close();
      expect(circuit.getState()).toBe('open');
    },
  );

  specTest(
    'a caller cancel before admission records nothing at all',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'a-caller-cancel-before-admission-writes-nothing-to-the-breaker',
    },
    () => {
      const circuit = breaker();
      const controller = new AbortController();
      const live = session({ breaker: circuit, callerSignal: controller.signal });
      live.dispatch();
      controller.abort();
      live.close();
      expect(circuit.getState()).toBe('closed');
      expect(circuit.allowRequest()).toBe(true);
    },
  );

  test('a local failure that never reached the provider records nothing', () => {
    const circuit = breaker();
    const live = session({ breaker: circuit });
    live.fail(new ClientCredentialError('no credential source'));
    live.close();
    expect(circuit.getState()).toBe('closed');
  });

  test('a provider answer the circuit policy does not count as a failure records a success', () => {
    const circuit = breaker();
    const live = session({ breaker: circuit, failureStatuses: [503] });
    live.dispatch();
    live.fail(new ClientFrameworkError({ service: 's', method: 'm', status: 404, code: 'not_found' }));
    live.close();
    expect(circuit.getState()).toBe('closed');
  });

  test('a circuit rejection records nothing, so a probe is never spent twice', () => {
    const circuit = breaker();
    const live = session({ breaker: circuit });
    live.dispatch();
    live.fail(new CircuitOpenError('open', 's', 'm'));
    live.close();
    expect(circuit.getState()).toBe('closed');
  });
});

describe('budgets', () => {
  test('the handshake budget ends an unadmitted session in a typed deadline', async () => {
    const live = session({ budgets: { ...BUDGETS, handshakeMs: 20 } });
    const failure = new Promise<Error>((resolve) => live.observer().onError(resolve));
    live.beginAttempt();
    expect(live.handshakeDeadline()).toBeGreaterThan(Date.now());
    await expect(failure).resolves.toBeInstanceOf(ClientDeadlineError);
  });

  specTest(
    'the handshake budget is released at admission, so a long stream is not cut by it',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'the-handshake-budget-is-released-at-admission',
    },
    async () => {
      const live = session({ budgets: { ...BUDGETS, handshakeMs: 20 } });
      let failed: Error | undefined;
      live.observer().onError((error) => {
        failed = error;
      });
      live.beginAttempt();
      live.admit();
      await Bun.sleep(45);
      expect(failed).toBeUndefined();
      expect(live.phase).toBe('admitted');
    },
  );

  test('the idle budget applies only after admission and is reset by activity', async () => {
    const live = session({ budgets: { ...BUDGETS, idleMs: 30 } });
    let failed: Error | undefined;
    live.observer().onError((error) => {
      failed = error;
    });
    await Bun.sleep(45);
    expect(failed).toBeUndefined();
    live.admit();
    await Bun.sleep(20);
    live.touchIdle();
    await Bun.sleep(20);
    expect(failed).toBeUndefined();
    await Bun.sleep(30);
    expect(failed).toBeInstanceOf(ClientDeadlineError);
  });

  test('the declared operation duration bounds the whole session when it is declared', async () => {
    const live = session({ budgets: { ...BUDGETS, sessionMs: 20 } });
    const failure = new Promise<Error>((resolve) => live.observer().onError(resolve));
    live.admit();
    await expect(failure).resolves.toBeInstanceOf(ClientDeadlineError);
  });

  specTest(
    'past the queue depth the stream ends with the declared queue error, never a silent drop',
    {
      feature: 'typescript/service-clients',
      requirement: 'stream-session-lifecycle',
      check: 'the-queue-bound-ends-the-stream-instead-of-dropping',
    },
    () => {
      const live = session<number>({ budgets: { ...BUDGETS, maxBufferedMessages: 2 } });
      let failed: Error | undefined;
      live.observer().onError((error) => {
        failed = error;
      });
      live.admit();
      live.deliver(1);
      live.deliver(2);
      live.deliver(3);
      expect(failed).toBeInstanceOf(ClientResponseContractError);
      expect(failed?.message).toContain('receive queue exceeds the declared maximum');
    },
  );
});

describe('credentials', () => {
  test('resolves twice, so an acquisition that expired while it blocked is re-resolved', async () => {
    let resolutions = 0;
    const live = session({
      credentials: async () => {
        resolutions += 1;
      },
    });
    await live.credentials();
    expect(resolutions).toBe(2);
  });

  test('invalidates the resolved credential exactly once', () => {
    let invalidations = 0;
    const live = session({
      invalidateCredentials: () => {
        invalidations += 1;
      },
    });
    live.invalidateCredentials();
    live.invalidateCredentials();
    expect(invalidations).toBe(1);
  });
});
