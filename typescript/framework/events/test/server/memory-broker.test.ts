import { describe, expect, it, beforeEach, afterEach } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { useContext, Uuid } from '@putnami/runtime';
import { TelemetryCollector, setCollector } from '@putnami/application';
import { MemoryBroker } from '../../src/server/memory-broker';
import { handler } from '../../src/handler/handler';
import { topic } from '../../src/topic/topic';
import type { EventContext } from '../../src/context';
import type { Message } from '../../src/topic/message';
import type { Envelope } from '../../src/transport/transport';
import { buildEnvelope } from '../../src/transport/transport';

const TestTopic = topic('test.event', { id: Uuid, value: String });

function makeEnvelope(overrides?: Partial<Envelope>): Envelope {
  return { ...buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'hello' }, overrides), ...overrides };
}

async function waitUntil(condition: () => boolean, timeoutMs = 200): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

describe('MemoryBroker', () => {
  let broker: MemoryBroker;

  beforeEach(async () => {
    broker = new MemoryBroker({ simulateDuplicates: false }); // Disable random duplicates for deterministic tests
    await broker.start();
  });

  afterEach(async () => {
    await broker.stop();
  });

  it('should deliver messages to subscribers', async () => {
    const received: Message<unknown>[] = [];

    const def = handler(TestTopic).handle(async () => {});
    await broker.subscribe(def, async (msg) => {
      received.push(msg);
    });

    const envelope = makeEnvelope();
    await broker.publish(TestTopic.name, envelope);
    await waitUntil(() => received.length === 1);

    expect(received.length).toBe(1);
    expect(received[0].topic).toBe('test.event');
    expect(received[0].id).toBe(envelope.id);
  });

  it('should round-robin competing consumers', async () => {
    const received1: Message<unknown>[] = [];
    const received2: Message<unknown>[] = [];

    const def1 = handler(TestTopic).handle(async () => {});
    const def2 = handler(TestTopic).handle(async () => {});

    await broker.subscribe(def1, async (msg) => {
      received1.push(msg);
    });
    await broker.subscribe(def2, async (msg) => {
      received2.push(msg);
    });

    // Publish 4 messages
    await Promise.all(Array.from({ length: 4 }, () => broker.publish(TestTopic.name, makeEnvelope())));
    await waitUntil(() => received1.length + received2.length === 4);

    // Round-robin: 2 each
    expect(received1.length).toBe(2);
    expect(received2.length).toBe(2);
  });

  it('should broadcast to all broadcast subscribers', async () => {
    const received1: Message<unknown>[] = [];
    const received2: Message<unknown>[] = [];

    const def1 = handler(TestTopic)
      .options({ distribution: 'broadcast' })
      .handle(async () => {});
    const def2 = handler(TestTopic)
      .options({ distribution: 'broadcast' })
      .handle(async () => {});

    await broker.subscribe(def1, async (msg) => {
      received1.push(msg);
    });
    await broker.subscribe(def2, async (msg) => {
      received2.push(msg);
    });

    await broker.publish(TestTopic.name, makeEnvelope());
    await waitUntil(() => received1.length === 1 && received2.length === 1);

    // Both should receive the message
    expect(received1.length).toBe(1);
    expect(received2.length).toBe(1);
  });

  it('should filter by attributes', async () => {
    const received: Message<unknown>[] = [];

    const def = handler(TestTopic)
      .filter({ attributes: { region: 'eu' } })
      .handle(async () => {});

    await broker.subscribe(def, async (msg) => {
      received.push(msg);
    });

    // Publish with matching attributes
    const matching = buildEnvelope(
      TestTopic,
      { id: crypto.randomUUID(), value: 'match' },
      {
        attributes: { region: 'eu' },
      },
    );
    await broker.publish(TestTopic.name, matching);

    // Publish with non-matching attributes
    const nonMatching = buildEnvelope(
      TestTopic,
      { id: crypto.randomUUID(), value: 'skip' },
      {
        attributes: { region: 'us' },
      },
    );
    await broker.publish(TestTopic.name, nonMatching);
    await waitUntil(() => received.length === 1);

    expect(received.length).toBe(1);
    expect((received[0].payload as { value: string }).value).toBe('match');
  });

  specTest(
    'should retry on handler failure with backoff',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-retry',
      check: 'a-failed-delivery-is-retried-with-backoff',
    },
    async () => {
      let attempts = 0;

      const def = handler(TestTopic)
        .options({ maxRetries: 3, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        attempts++;
        if (attempts < 3) {
          throw new Error('Simulated failure');
        }
      });

      const originalSetTimeout = globalThis.setTimeout;
      globalThis.setTimeout = ((callback: TimerHandler, _delay?: number, ...args: unknown[]) => {
        queueMicrotask(() => {
          if (typeof callback === 'function') {
            callback(...args);
          }
        });
        return 0 as ReturnType<typeof setTimeout>;
      }) as typeof setTimeout;

      try {
        await broker.publish(TestTopic.name, makeEnvelope());
        await waitUntil(() => attempts === 3);
      } finally {
        globalThis.setTimeout = originalSetTimeout;
      }

      expect(attempts).toBe(3);
    },
  );

  specTest(
    'should drop messages when no subscribers exist',
    {
      feature: 'typescript/event-messaging',
      requirement: 'dead-letter-or-drop',
      check: 'a-message-with-no-subscriber-is-dropped',
    },
    async () => {
      // Should not throw
      await broker.publish(TestTopic.name, makeEnvelope());
    },
  );

  specTest(
    'should throw when publishing to stopped broker',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'a-stopped-broker-refuses-new-publishes',
    },
    async () => {
      await broker.stop();

      expect(broker.publish(TestTopic.name, makeEnvelope())).rejects.toThrow('Broker is not running');
    },
  );

  specTest(
    'should include message metadata',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-retry',
      check: 'the-handler-sees-the-attempt-number',
    },
    async () => {
      const received: Message<unknown>[] = [];

      const def = handler(TestTopic).handle(async () => {});
      await broker.subscribe(def, async (msg) => {
        received.push(msg);
      });

      const envelope = makeEnvelope();
      await broker.publish(TestTopic.name, envelope);
      await waitUntil(() => received.length === 1);

      const msg = received[0];
      expect(msg.id).toBe(envelope.id);
      expect(msg.topic).toBe('test.event');
      expect(msg.channel).toBe(envelope.channel);
      expect(msg.timestamp).toBeInstanceOf(Date);
      expect(msg.attempt).toBe(1);
      expect(msg.traceId).toBeDefined();
    },
  );

  specTest(
    'should run handler inside runInContext with event context',
    {
      feature: 'typescript/event-messaging',
      requirement: 'isolated-invocation',
      check: 'a-handler-runs-inside-its-own-event-context',
    },
    async () => {
      let capturedContext: EventContext | undefined;

      const def = handler(TestTopic).handle(async () => {});
      await broker.subscribe(def, async () => {
        capturedContext = useContext<EventContext>();
      });

      const envelope = makeEnvelope();
      await broker.publish(TestTopic.name, envelope);
      await waitUntil(() => capturedContext !== undefined);

      expect(capturedContext).toBeDefined();
      expect(capturedContext?.eventTopic).toBe('test.event');
      expect(capturedContext?.eventMessageId).toBe(envelope.id);
      expect(capturedContext?.eventAttempt).toBe(1);
      expect(capturedContext?.traceId).toBe(envelope.traceId);
    },
  );

  it('should emit telemetry counters on success', async () => {
    const collector = new TelemetryCollector();
    setCollector(collector);

    const def = handler(TestTopic).handle(async () => {});
    await broker.subscribe(def, async () => {});

    await broker.publish(TestTopic.name, makeEnvelope());
    await waitUntil(() => !collector.isEmpty());

    const buckets = collector.drainAll();
    const counters = Object.assign({}, ...buckets.map((b) => b.counters));
    expect(counters['events.handle.test.event.success']).toBe(1);

    const histograms = Object.assign({}, ...buckets.map((b) => b.histograms));
    expect(histograms['events.handle.test.event.duration']).toBeDefined();
    expect(histograms['events.handle.test.event.duration'].count).toBe(1);

    setCollector(undefined);
  });

  it('should emit telemetry counters on failure', async () => {
    const collector = new TelemetryCollector();
    setCollector(collector);

    const def = handler(TestTopic)
      .options({ maxRetries: 1, timeout: 0 })
      .handle(async () => {});
    await broker.subscribe(def, async () => {
      throw new Error('boom');
    });

    await broker.publish(TestTopic.name, makeEnvelope());
    await waitUntil(() => !collector.isEmpty());

    const buckets = collector.drainAll();
    const counters = Object.assign({}, ...buckets.map((b) => b.counters));
    expect(counters['events.handle.test.event.failure']).toBe(1);
    expect(counters['events.handle.test.event.dlq']).toBe(1);

    setCollector(undefined);
  });

  it('should expose manual ack and nack controls on messages', async () => {
    let nackError: Error | undefined;

    const def = handler(TestTopic)
      .options({ ack: 'manual', maxRetries: 1, timeout: 0 })
      .handle(async () => {});

    await broker.subscribe(def, async (msg) => {
      msg.ack();
      try {
        msg.nack('manual failure');
      } catch (error) {
        nackError = error as Error;
      }
    });

    await broker.publish(TestTopic.name, makeEnvelope());
    await waitUntil(() => nackError !== undefined);

    expect(nackError?.message).toBe('manual failure');
  });

  specTest(
    'should retry manual handlers that return without acking',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-retry',
      check: 'a-manual-handler-that-never-acks-is-retried',
    },
    async () => {
      let attempts = 0;

      const def = handler(TestTopic)
        .options({ ack: 'manual', maxRetries: 2, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        attempts++;
        if (attempts === 2) {
          msg.ack();
        }
      });

      const originalSetTimeout = globalThis.setTimeout;
      globalThis.setTimeout = ((callback: TimerHandler, _delay?: number, ...args: unknown[]) => {
        queueMicrotask(() => {
          if (typeof callback === 'function') {
            callback(...args);
          }
        });
        return 0 as ReturnType<typeof setTimeout>;
      }) as typeof setTimeout;

      try {
        await broker.publish(TestTopic.name, makeEnvelope());
        await waitUntil(() => attempts === 2);
      } finally {
        globalThis.setTimeout = originalSetTimeout;
      }

      expect(attempts).toBe(2);
    },
  );

  specTest(
    'should invoke DLQ subscribers even when the DLQ handler fails',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-failing-dlq-handler-is-still-invoked-and-contained',
    },
    async () => {
      let dlqCalls = 0;
      const DlqTopic = topic('test.event.dlq', { id: Uuid, value: String });

      const failingDef = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(failingDef, async () => {
        throw new Error('primary failure');
      });
      await broker.subscribe(dlqDef, async () => {
        dlqCalls++;
        throw new Error('dlq failure');
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => dlqCalls === 1);

      expect(dlqCalls).toBe(1);
    },
  );

  specTest(
    'should deliver DLQ messages on the dead-letter topic with failure metadata',
    {
      feature: 'typescript/event-messaging',
      requirement: 'dead-letter-or-drop',
      check: 'an-exhausted-delivery-reaches-the-dlq-topic-with-failure-metadata',
    },
    async () => {
      let dlqMessage: Message<unknown> | undefined;
      const DlqTopic = topic('test.event.dlq', { id: Uuid, value: String });

      const failingDef = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic).handle(async () => {});

      await broker.subscribe(failingDef, async () => {
        throw new Error('primary failure');
      });
      await broker.subscribe(dlqDef, async (msg) => {
        dlqMessage = msg;
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => dlqMessage !== undefined);

      expect(dlqMessage?.topic).toBe('test.event.dlq');
      expect(dlqMessage?.attributes['dlq.original_topic']).toBe('test.event');
      expect(dlqMessage?.attributes['dlq.error']).toBe('primary failure');
      expect(dlqMessage?.attempt).toBe(1);
    },
  );

  specTest(
    'should deliver valid payloads to the handler',
    {
      feature: 'typescript/event-messaging',
      requirement: 'payload-validation',
      check: 'a-valid-payload-reaches-the-handler',
    },
    async () => {
      const received: Message<unknown>[] = [];

      const def = handler(TestTopic).handle(async () => {});
      await broker.subscribe(def, async (msg) => {
        received.push(msg);
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => received.length === 1);

      expect(received.length).toBe(1);
      expect((received[0].payload as { value: string }).value).toBe('hello');
    },
  );

  specTest(
    'should reject inbound payloads that violate the topic schema (handler not invoked)',
    {
      feature: 'typescript/event-messaging',
      requirement: 'payload-validation',
      check: 'a-schema-invalid-payload-never-reaches-the-handler',
    },
    async () => {
      let handlerCalls = 0;

      const def = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        handlerCalls++;
      });

      // Envelope whose payload does not match the schema (id is not a Uuid, value missing).
      // Simulates a malformed/malicious payload from a cross-language producer.
      const invalid = makeEnvelope({ payload: { id: 'not-a-uuid' } as unknown as Record<string, unknown> });
      await broker.publish(TestTopic.name, invalid);

      // Wait long enough for the (failed) delivery to settle.
      await Bun.sleep(20);

      expect(handlerCalls).toBe(0);
    },
  );

  specTest(
    'should route schema-invalid payloads to the DLQ',
    {
      feature: 'typescript/event-messaging',
      requirement: 'payload-validation',
      check: 'a-schema-invalid-payload-takes-the-ordinary-failure-path-to-the-dlq',
    },
    async () => {
      let dlqMessage: Message<unknown> | undefined;
      // Dead-letter topics intentionally carry off-contract payloads, so they
      // use an empty (permissive) schema that accepts any shape.
      const DlqTopic = topic('test.event.dlq', {});

      const def = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic).handle(async () => {});

      await broker.subscribe(def, async () => {});
      await broker.subscribe(dlqDef, async (msg) => {
        dlqMessage = msg;
      });

      const invalid = makeEnvelope({ payload: { id: 'not-a-uuid' } as unknown as Record<string, unknown> });
      await broker.publish(TestTopic.name, invalid);
      await waitUntil(() => dlqMessage !== undefined);

      expect(dlqMessage?.topic).toBe('test.event.dlq');
      expect(dlqMessage?.attributes['dlq.original_topic']).toBe('test.event');
      expect(dlqMessage?.attributes['dlq.error']).toContain('Invalid payload');
    },
  );

  specTest(
    'should expose timeout abort signals to handlers',
    {
      feature: 'typescript/event-messaging',
      requirement: 'isolated-invocation',
      check: 'a-timeout-abort-signal-is-visible-to-the-handler',
    },
    async () => {
      let aborted = false;

      const def = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 1 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        msg.signal.addEventListener('abort', () => {
          aborted = true;
        });
        await Bun.sleep(10);
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => aborted);

      expect(aborted).toBe(true);
    },
  );

  specTest(
    'should not crash the worker when retry re-enqueue throws (routes to DLQ instead)',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-refused-retry-re-enqueue-is-routed-to-the-dlq',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      let dlqMessage: Message<unknown> | undefined;
      const DlqTopic = topic('test.event.dlq', { id: Uuid, value: String });

      const failingDef = handler(TestTopic)
        .options({ maxRetries: 3, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic).handle(async () => {});

      await broker.subscribe(failingDef, async () => {
        throw new Error('primary failure');
      });
      await broker.subscribe(dlqDef, async (msg) => {
        dlqMessage = msg;
      });

      // Force the retry re-enqueue (which runs inside the bare setTimeout
      // callback) to throw, simulating a saturated queue with overflow:'throw'.
      const internals = broker as unknown as {
        enqueueDelivery: (envelope: Envelope, sub: unknown) => void;
      };
      const original = internals.enqueueDelivery.bind(broker);
      let calls = 0;
      internals.enqueueDelivery = (envelope: Envelope, sub: unknown) => {
        calls++;
        // First call = initial delivery (let it run so the handler fails and
        // schedules a retry). Second call = retry re-enqueue → throw.
        if (calls === 2) {
          throw new Error('Queue limit reached for retry');
        }
        original(envelope, sub);
      };

      // Fire the retry timer synchronously.
      const originalSetTimeout = globalThis.setTimeout;
      globalThis.setTimeout = ((callback: TimerHandler, _delay?: number, ...args: unknown[]) => {
        queueMicrotask(() => {
          if (typeof callback === 'function') {
            callback(...args);
          }
        });
        return 0 as ReturnType<typeof setTimeout>;
      }) as typeof setTimeout;

      try {
        await broker.publish(TestTopic.name, makeEnvelope());
        // The throwing retry must be caught and routed to the DLQ subscriber.
        await waitUntil(() => dlqMessage !== undefined);
        // Give any stray rejection a tick to surface.
        await Bun.sleep(5);
      } finally {
        globalThis.setTimeout = originalSetTimeout;
        internals.enqueueDelivery = original;
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);
      expect(dlqMessage).toBeDefined();
      expect(dlqMessage?.topic).toBe('test.event.dlq');
      expect(dlqMessage?.attributes['dlq.error']).toBe('Queue limit reached for retry');

      // Broker stays usable: a fresh message is still delivered.
      let delivered = false;
      const okDef = handler(topic('test.ok', { id: Uuid })).handle(async () => {});
      await broker.subscribe(okDef, async () => {
        delivered = true;
      });
      await broker.publish('test.ok', buildEnvelope(topic('test.ok', { id: Uuid }), { id: crypto.randomUUID() }));
      await waitUntil(() => delivered);
      expect(delivered).toBe(true);
    },
  );

  specTest(
    'should route a scope-creation failure to the DLQ without losing the message or leaking a rejection',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-scope-creation-failure-is-routed-not-left-as-a-rejection',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      let primaryHandlerCalls = 0;
      let dlqMessage: Message<unknown> | undefined;
      const DlqTopic = topic('test.event.dlq', { id: Uuid, value: String });

      // The DI scope factory rejects on the FIRST call (the primary delivery, before
      // its handler can run) and resolves afterwards (so the routed DLQ delivery can
      // be scoped and reach its subscriber). With maxRetries:1 the primary's
      // attempt-1 failure routes straight to the DLQ.
      let scopeCalls = 0;
      broker.setScopeFactory(() => {
        scopeCalls++;
        if (scopeCalls === 1) {
          return Promise.reject(new Error('scope creation failed'));
        }
        return Promise.resolve({ scope: {} as never, close: async () => {} });
      });

      const failingDef = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic).handle(async () => {});

      await broker.subscribe(failingDef, async () => {
        primaryHandlerCalls++;
      });
      await broker.subscribe(dlqDef, async (msg) => {
        dlqMessage = msg;
      });

      const envelope = makeEnvelope();
      try {
        await broker.publish(TestTopic.name, envelope);
        // The scope failure must be routed to the DLQ subscriber, not dropped.
        await waitUntil(() => dlqMessage !== undefined);
        // Give any stray rejection a tick to surface.
        await Bun.sleep(5);
      } finally {
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      // No unhandled rejection / uncaught exception escaped the fire-and-forget
      // delivery — the pre-fix code rejected runHandler() with no .catch(), losing
      // the message and surfacing an unhandled rejection that can crash the worker.
      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);
      // The primary handler never ran because its scope could not be created.
      expect(primaryHandlerCalls).toBe(0);
      // The message was preserved and delivered to the DLQ with the scope error.
      expect(dlqMessage).toBeDefined();
      expect(dlqMessage?.topic).toBe('test.event.dlq');
      expect(dlqMessage?.attributes['dlq.original_topic']).toBe('test.event');
      expect(dlqMessage?.attributes['dlq.error']).toBe('scope creation failed');
    },
  );

  specTest(
    'should capture a dead-letter when scope creation keeps failing and no DLQ subscriber exists',
    {
      feature: 'typescript/event-messaging',
      requirement: 'no-silent-loss',
      check: 'a-repeated-scope-failure-with-no-dlq-subscriber-is-captured',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      // Scope creation always rejects; maxRetries:1 routes the attempt-1 failure to
      // the DLQ, and with dlq:true but no `<topic>.dlq` subscriber it is captured
      // in the dead-letter buffer instead of being silently lost.
      broker.setScopeFactory(() => Promise.reject(new Error('scope creation failed')));

      const def = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {});

      const envelope = makeEnvelope();
      try {
        await broker.publish(TestTopic.name, envelope);
        await waitUntil(() => broker.getDeadLetters().length === 1);
        await Bun.sleep(5);
      } finally {
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);

      const deadLetters = broker.getDeadLetters();
      expect(deadLetters.length).toBe(1);
      expect(deadLetters[0].envelope.id).toBe(envelope.id);
      expect(deadLetters[0].error).toBe('scope creation failed');
    },
  );

  specTest(
    'should not leak an unhandled rejection when scope close() fails after a successful handler',
    {
      feature: 'typescript/event-messaging',
      requirement: 'isolated-invocation',
      check: 'a-scope-close-failure-after-success-leaks-no-rejection',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      let handlerCalls = 0;
      // Scope creation succeeds, the handler runs, but close() rejects in dispatch's
      // finally block — a residual rejection that the dispatch-level routing cannot
      // catch (it fires after onFailure would have run). The broker's defensive
      // .catch() must absorb it so it never becomes an unhandled rejection.
      broker.setScopeFactory(() =>
        Promise.resolve({
          scope: {} as never,
          close: async () => {
            throw new Error('scope close failed');
          },
        }),
      );

      const def = handler(TestTopic)
        .options({ maxRetries: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        handlerCalls++;
      });

      try {
        await broker.publish(TestTopic.name, makeEnvelope());
        await waitUntil(() => handlerCalls === 1);
        // Give the close() rejection a tick to surface as unhandled if unguarded.
        await Bun.sleep(5);
      } finally {
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      // The handler succeeded, yet the close() failure must not escape the
      // fire-and-forget delivery as an unhandled rejection (pre-fix: no .catch()).
      expect(handlerCalls).toBe(1);
      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);
    },
  );

  specTest(
    'should not crash the worker when a retry failure cannot be enqueued to the DLQ',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-dlq-enqueue-failure-does-not-crash-the-worker',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      const DlqTopic = topic('test.event.dlq', { id: Uuid, value: String });

      const failingDef = handler(TestTopic)
        .options({ maxRetries: 3, timeout: 0 })
        .handle(async () => {});
      const dlqDef = handler(DlqTopic).handle(async () => {});

      await broker.subscribe(failingDef, async () => {
        throw new Error('primary failure');
      });
      await broker.subscribe(dlqDef, async () => {});

      const internals = broker as unknown as {
        enqueueDelivery: (envelope: Envelope, sub: unknown) => void;
      };
      const original = internals.enqueueDelivery.bind(broker);
      let calls = 0;
      internals.enqueueDelivery = (envelope: Envelope, sub: unknown) => {
        calls++;
        if (calls === 2) {
          throw new Error('Queue limit reached for retry');
        }
        if (calls === 3) {
          throw new Error('Queue limit reached for DLQ');
        }
        original(envelope, sub);
      };

      const originalSetTimeout = globalThis.setTimeout;
      globalThis.setTimeout = ((callback: TimerHandler, _delay?: number, ...args: unknown[]) => {
        queueMicrotask(() => {
          if (typeof callback === 'function') {
            callback(...args);
          }
        });
        return 0 as ReturnType<typeof setTimeout>;
      }) as typeof setTimeout;

      try {
        await broker.publish(TestTopic.name, makeEnvelope());
        await waitUntil(() => calls >= 3);
        await Bun.sleep(5);
      } finally {
        globalThis.setTimeout = originalSetTimeout;
        internals.enqueueDelivery = original;
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);
    },
  );

  specTest(
    'should enforce per-handler concurrency and queue limits',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-concurrency',
      check: 'a-handler-admits-at-most-its-configured-concurrency',
    },
    async () => {
      const started: string[] = [];
      const finished: string[] = [];
      let releaseFirst: () => void;
      const firstBlocked = new Promise<void>((resolve) => {
        releaseFirst = resolve;
      });

      const def = handler(TestTopic)
        .options({ concurrency: 1, queueLimit: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        const value = (msg.payload as { value: string }).value;
        started.push(value);
        if (value === 'first') {
          await firstBlocked;
        }
        finished.push(value);
      });

      await broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'first' } }));
      await waitUntil(() => started.includes('first'));
      await broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'second' } }));

      await expect(
        broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'third' } })),
      ).rejects.toThrow('Queue limit reached');

      releaseFirst?.();
      await waitUntil(() => finished.includes('second'));

      expect(started).toEqual(['first', 'second']);
    },
  );

  specTest(
    'should drain concurrency-queued messages on stop before resolving',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'queued-deliveries-drain-before-stop-resolves',
    },
    async () => {
      const processed: string[] = [];
      let releaseFirst: () => void;
      const firstBlocked = new Promise<void>((resolve) => {
        releaseFirst = resolve;
      });

      // concurrency 1 → at most one handler runs at a time; the rest queue.
      // queueLimit 0 → unlimited queue, so no message is rejected on publish.
      const def = handler(TestTopic)
        .options({ concurrency: 1, queueLimit: 0, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        const value = (msg.payload as { value: string }).value;
        if (value === 'm0') {
          // Hold the first delivery in-flight so the remaining four queue up.
          await firstBlocked;
        }
        processed.push(value);
      });

      const ids = ['m0', 'm1', 'm2', 'm3', 'm4'];
      await Promise.all(
        ids.map((value) =>
          broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value } })),
        ),
      );

      // Let the first delivery reach in-flight (it parks on firstBlocked) and the
      // remaining four settle into the concurrency queue.
      await Bun.sleep(5);

      // Release the in-flight handler, then stop(). A graceful drain must process
      // every admitted message — including the queued ones — before resolving.
      releaseFirst?.();
      await broker.stop();

      expect(processed).toEqual(ids);
    },
  );

  specTest(
    'should bound drain by drainTimeout when a queued handler never settles',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'the-drain-is-bounded-by-the-drain-timeout',
    },
    async () => {
      const fastBroker = new MemoryBroker({ simulateDuplicates: false, drainTimeout: 30 });
      await fastBroker.start();

      const processed: string[] = [];
      let releaseHang: () => void;
      const hang = new Promise<void>((resolve) => {
        releaseHang = resolve;
      });

      const def = handler(TestTopic)
        .options({ concurrency: 1, queueLimit: 0, timeout: 0 })
        .handle(async () => {});

      await fastBroker.subscribe(def, async (msg) => {
        const value = (msg.payload as { value: string }).value;
        if (value === 'm0') {
          await hang; // never released before stop() → forces drain timeout
        }
        processed.push(value);
      });

      await Promise.all(
        ['m0', 'm1', 'm2'].map((value) =>
          fastBroker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value } })),
        ),
      );
      await Bun.sleep(5);

      const startedAt = Date.now();
      await fastBroker.stop(); // must not hang forever
      const elapsed = Date.now() - startedAt;

      expect(elapsed).toBeLessThan(500);
      // The hung in-flight handler never finished, so its queued followers stay
      // unprocessed — but stop() still returns bounded by drainTimeout.
      expect(processed).not.toContain('m0');

      releaseHang?.();
    },
  );

  // ---------------------------------------------------------------------------
  // Concurrency and races — interleaving behaviours the broker must get right.
  //
  // These exercise the highest-risk paths (loss/duplication under concurrent
  // dispatch, retries racing shutdown, and overflow on the retry path) without
  // monkeypatching setTimeout, so real backoff/jitter ordering is exercised.
  // ---------------------------------------------------------------------------

  specTest(
    'should not lose or duplicate messages under concurrent publish (single consumer)',
    {
      feature: 'typescript/event-messaging',
      requirement: 'at-least-once',
      check: 'a-message-is-neither-lost-nor-duplicated-under-concurrent-publish',
    },
    async () => {
      const received = new Set<string>();
      let count = 0;

      const def = handler(TestTopic).handle(async () => {});
      await broker.subscribe(def, async (msg) => {
        count++;
        received.add((msg.payload as { value: string }).value);
        // Yield to interleave deliveries on the event loop.
        await Bun.sleep(0);
      });

      const ids = Array.from({ length: 200 }, (_, i) => `m${i}`);
      // Fire every publish without awaiting between them, so deliveries race.
      await Promise.all(
        ids.map((value) =>
          broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value } })),
        ),
      );
      await waitUntil(() => count === ids.length, 2000);

      // Exactly once each: no loss (size === total) and no duplication (count === size).
      expect(count).toBe(ids.length);
      expect(received.size).toBe(ids.length);
    },
  );

  specTest(
    'should distribute evenly across competing consumers under parallel publish',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-concurrency',
      check: 'competing-consumers-share-the-load-under-parallel-publish',
    },
    async () => {
      const consumerCount = 4;
      const counts = Array.from({ length: consumerCount }, () => 0);
      const total = 200; // multiple of the consumer count → exact even split

      await Promise.all(
        counts.map((_, index) => {
          const def = handler(TestTopic).handle(async () => {});
          return broker.subscribe(def, async () => {
            counts[index]++;
            await Bun.sleep(0);
          });
        }),
      );

      await Promise.all(
        Array.from({ length: total }, (_, i) =>
          broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: `m${i}` } })),
        ),
      );
      await waitUntil(() => counts.reduce((a, b) => a + b, 0) === total, 2000);

      // No loss across the round-robin counter, and a deterministic even split:
      // the counter advances once per published message regardless of delivery
      // timing, so each of the 4 consumers gets exactly total/4.
      expect(counts.reduce((a, b) => a + b, 0)).toBe(total);
      expect(counts).toEqual(Array.from({ length: consumerCount }, () => total / consumerCount));
    },
  );

  specTest(
    'should deliver a real (non-mocked) backoff retry that fires while running',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-retry',
      check: 'a-scheduled-backoff-retry-really-fires',
    },
    async () => {
      let attempts = 0;
      const succeededAt: number[] = [];

      // maxBackoff: 1 keeps the real setTimeout delay near 1ms so the genuine
      // backoff/jitter path runs fast instead of being monkeypatched away.
      const def = handler(TestTopic)
        .options({ maxRetries: 5, maxBackoff: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        attempts++;
        if (attempts < 3) {
          throw new Error('transient failure');
        }
        succeededAt.push(attempts);
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => succeededAt.length === 1, 2000);

      // Two real retries elapsed before the third attempt succeeded.
      expect(attempts).toBe(3);
      expect(succeededAt).toEqual([3]);
    },
  );

  specTest(
    'should not deliver a retry scheduled before stop() once the broker has stopped',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'a-retry-scheduled-before-stop-is-not-delivered-afterwards',
    },
    async () => {
      let attempts = 0;

      // First attempt fails and schedules a real (~1ms) backoff retry; we stop()
      // the broker inside that window so the retry timer fires against a stopped
      // broker. scheduleRetry must bail when not running — no extra delivery.
      const def = handler(TestTopic)
        .options({ maxRetries: 5, maxBackoff: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        attempts++;
        throw new Error('always fails');
      });

      await broker.publish(TestTopic.name, makeEnvelope());
      await waitUntil(() => attempts === 1);

      // Stop immediately, then give the pending retry timer ample time to fire.
      await broker.stop();
      await Bun.sleep(20);

      // The retry that was queued before stop() must not have been delivered.
      expect(attempts).toBe(1);
    },
  );

  specTest(
    'should capture a dead-letter when retries exhaust with dlq enabled but no subscriber',
    {
      feature: 'typescript/event-messaging',
      requirement: 'no-silent-loss',
      check: 'an-exhausted-delivery-with-no-dlq-subscriber-is-captured',
    },
    async () => {
      const def = handler(TestTopic)
        .options({ maxRetries: 2, maxBackoff: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        throw new Error('permanent failure');
      });

      const envelope = makeEnvelope();
      await broker.publish(TestTopic.name, envelope);
      await waitUntil(() => broker.getDeadLetters().length === 1, 2000);

      const deadLetters = broker.getDeadLetters();
      expect(deadLetters.length).toBe(1);
      expect(deadLetters[0].envelope.id).toBe(envelope.id);
      expect(deadLetters[0].error).toBe('permanent failure');
      expect(deadLetters[0].reason).toBe('no-dlq-subscriber');

      // drainDeadLetters returns and clears the buffer.
      const drained = broker.drainDeadLetters();
      expect(drained.length).toBe(1);
      expect(broker.getDeadLetters().length).toBe(0);
    },
  );

  specTest(
    'should drop (not throw) on the retry path when the queue overflows with overflow:drop',
    {
      feature: 'typescript/event-messaging',
      requirement: 'bounded-concurrency',
      check: 'an-overflow-under-drop-policy-drops-instead-of-throwing',
    },
    async () => {
      const uncaught: unknown[] = [];
      const unhandled: unknown[] = [];
      const onUncaught = (err: unknown) => uncaught.push(err);
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('uncaughtException', onUncaught);
      process.on('unhandledRejection', onUnhandled);

      let releaseFirst: () => void;
      const firstBlocked = new Promise<void>((resolve) => {
        releaseFirst = resolve;
      });
      const handled: string[] = [];

      // concurrency 1 + queueLimit 1 + overflow:'drop'. The first delivery parks
      // in-flight; a second fills the single queue slot. A failing message then
      // retries (real ~1ms backoff) and, finding the queue full, must be dropped
      // rather than throwing out of the bare retry timer callback.
      const def = handler(TestTopic)
        .options({ concurrency: 1, queueLimit: 1, overflow: 'drop', maxRetries: 3, maxBackoff: 1, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        const value = (msg.payload as { value: string }).value;
        handled.push(value);
        if (value === 'block') {
          await firstBlocked;
        }
        if (value === 'fail') {
          throw new Error('fail to trigger retry');
        }
      });

      try {
        // 'block' goes in-flight and parks.
        await broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'block' } }));
        await waitUntil(() => handled.includes('block'));
        // 'fail' fills the single queue slot, then runs once 'block' is released.
        await broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'fail' } }));
        // 'filler' would overflow on the initial publish (queue already has 'fail'),
        // so with overflow:'drop' the publish itself silently drops it.
        await broker.publish(TestTopic.name, makeEnvelope({ payload: { id: crypto.randomUUID(), value: 'filler' } }));

        releaseFirst?.();
        // Let 'fail' run, fail, and schedule its real retry; the retry re-enqueue
        // must drop quietly without surfacing an uncaught error.
        await Bun.sleep(40);
      } finally {
        process.off('uncaughtException', onUncaught);
        process.off('unhandledRejection', onUnhandled);
      }

      // The key guarantee: a retry overflow with overflow:'drop' never escapes as
      // an uncaught exception / unhandled rejection that would crash the worker.
      expect(uncaught).toEqual([]);
      expect(unhandled).toEqual([]);
      expect(handled).toContain('block');
      expect(handled).toContain('fail');
    },
  );
});

// Deterministic coverage for the most race-prone lifecycle path: stop()'s
// drain loop interleaving with FailureRouter retry timers and parked
// concurrency-queue deliveries.
describe('MemoryBroker shutdown drain vs retry race', () => {
  const TestTopic = topic('race.event', { id: Uuid, value: String });

  function makeEnvelope(value: string): Envelope {
    return buildEnvelope(TestTopic, { id: crypto.randomUUID(), value });
  }

  specTest(
    'cancels a pending retry on stop() and never redelivers',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'a-pending-retry-is-cancelled-by-stop-and-never-redelivered',
    },
    async () => {
      const broker = new MemoryBroker({ simulateDuplicates: false, drainTimeout: 1000 });
      await broker.start();

      let attempts = 0;
      // maxBackoff 200ms: long enough that the retry timer is still pending
      // when stop() runs, short enough to assert non-delivery afterwards.
      const def = handler(TestTopic)
        .options({ maxRetries: 3, maxBackoff: 200, timeout: 0 })
        .handle(async () => {});

      await broker.subscribe(def, async () => {
        attempts++;
        throw new Error('handler still failing while stop() drains');
      });

      await broker.publish(TestTopic.name, makeEnvelope('m0'));
      await waitUntil(() => attempts === 1);

      // The failure above scheduled a retry ~200ms out; stop now, before it fires.
      await broker.stop();

      // Wait past the backoff window: a surviving timer would redeliver here.
      await Bun.sleep(350);
      expect(attempts).toBe(1);
    },
  );

  specTest(
    'drains queued deliveries despite a failing in-flight handler, without waiting for its retry',
    {
      feature: 'typescript/event-messaging',
      requirement: 'graceful-drain',
      check: 'a-failing-in-flight-handler-does-not-hold-up-the-drain',
    },
    async () => {
      const broker = new MemoryBroker({ simulateDuplicates: false, drainTimeout: 1000 });
      await broker.start();

      const processed: string[] = [];
      let failures = 0;
      let releaseFirst: (() => void) | undefined;
      const firstStarted = new Promise<void>((resolve) => {
        releaseFirst = resolve;
      });

      const def = handler(TestTopic)
        .options({ concurrency: 1, queueLimit: 0, timeout: 0, maxRetries: 3, maxBackoff: 200 })
        .handle(async () => {});

      await broker.subscribe(def, async (msg) => {
        const value = (msg.payload as { value: string }).value;
        if (value === 'm0') {
          releaseFirst?.();
          failures++;
          throw new Error('in-flight failure during drain');
        }
        processed.push(value);
      });

      // m0 occupies the single concurrency slot and fails; m1/m2 park in the queue.
      await broker.publish(TestTopic.name, makeEnvelope('m0'));
      await firstStarted;
      await broker.publish(TestTopic.name, makeEnvelope('m1'));
      await broker.publish(TestTopic.name, makeEnvelope('m2'));

      const stopStart = Date.now();
      await broker.stop();
      const stopDuration = Date.now() - stopStart;

      // Every admitted delivery was processed exactly once during the drain...
      expect(processed.sort()).toEqual(['m1', 'm2']);
      // ...the drain did not stall on m0's pending retry timer...
      expect(stopDuration).toBeLessThan(750);
      // ...and the cancelled retry never fires after shutdown.
      await Bun.sleep(350);
      expect(failures).toBe(1);
      expect(processed.length).toBe(2);
    },
  );
});

// `simulateDuplicates` is the developer-facing half of at-least-once: every
// other test in this file turns it OFF for determinism, so the behaviour it
// exists for — surfacing a non-idempotent handler before production does —
// had no protecting test. Math.random is pinned so the 2% sample is a
// certainty rather than a 1-in-50 flake.
describe('MemoryBroker duplicate simulation', () => {
  const realRandom = Math.random;

  afterEach(() => {
    Math.random = realRandom;
  });

  specTest(
    'redelivers a handled message when duplicate simulation is opted into',
    {
      feature: 'typescript/event-messaging',
      requirement: 'at-least-once',
      check: 'an-opted-in-broker-redelivers-a-handled-message',
    },
    async () => {
      Math.random = () => 0; // inside the 2% sample
      const broker = new MemoryBroker({ simulateDuplicates: true });
      await broker.start();
      try {
        const attempts: number[] = [];
        const def = handler(TestTopic).handle(async () => {});
        await broker.subscribe(def, async (msg) => {
          attempts.push(msg.attempt);
        });

        const envelope = makeEnvelope();
        await broker.publish(TestTopic.name, envelope);
        await waitUntil(() => attempts.length === 2);

        expect(attempts.length).toBe(2);
        // The redelivery is a distinct attempt, so a handler that inspects
        // `attempt` can tell the duplicate from the original.
        expect(attempts[1]).toBe(attempts[0] + 1);
      } finally {
        await broker.stop();
      }
    },
  );

  specTest(
    'never redelivers a handled message when duplicate simulation is off',
    {
      feature: 'typescript/event-messaging',
      requirement: 'at-least-once',
      check: 'duplicate-simulation-is-off-unless-opted-into',
    },
    async () => {
      Math.random = () => 0; // the sample would hit if the opt-in were ignored
      const broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();
      try {
        const received: Message<unknown>[] = [];
        const def = handler(TestTopic).handle(async () => {});
        await broker.subscribe(def, async (msg) => {
          received.push(msg);
        });

        await broker.publish(TestTopic.name, makeEnvelope());
        await waitUntil(() => received.length === 1);
        await Bun.sleep(20);

        expect(received.length).toBe(1);
      } finally {
        await broker.stop();
      }
    },
  );
});
