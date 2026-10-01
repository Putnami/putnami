import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resetDefaultLogger, setRootLogger, useContext, Uuid } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import { MemoryLogger } from '@putnami/runtime/testing';
import { handler } from '../../src/handler/handler';
import { topic } from '../../src/topic/topic';
import { createTransportMessage } from '../../src/transport/message-utils';
import { dispatchToHandler } from '../../src/transport/dispatch';
import { buildEnvelope } from '../../src/transport/transport';

const TestTopic = topic('test.logging', { id: Uuid });

afterEach(() => {
  resetDefaultLogger();
});

describe('dispatch logging', () => {
  specTest(
    'emits one completion entry with delivery details in log context',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'a-completed-delivery-emits-one-completion-entry',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      const definition = handler(TestTopic)
        .options({ timeout: 0 })
        .handle(async () => {});
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-1' });
      const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

      await dispatchToHandler(message, ackState, abortController, envelope, definition, async () => {});

      expect(logger.entries).toHaveLength(1);
      expect(logger.entries[0]?.level).toBe('info');
      // Pinned logger name of the contract: dots, never colons.
      expect(logger.entries[0]?.logger).toBe('events.handler');
      expect(logger.entries[0]?.message).toBe('message handled');
      expect(logger.entries[0]?.context?.['event']).toEqual({
        topic: 'test.logging',
        messageId: 'message-1',
        attempt: 1,
        outcome: 'success',
        durationMs: expect.any(Number),
      });
    },
  );

  specTest(
    'emits one failure entry with the same completion context',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'a-failed-delivery-emits-one-failure-entry',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      const definition = handler(TestTopic)
        .options({ timeout: 0 })
        .handle(async () => {});
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-2' });
      const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

      await expect(
        dispatchToHandler(message, ackState, abortController, envelope, definition, async () => {
          throw new Error('handler failed');
        }),
      ).rejects.toThrow('handler failed');

      expect(logger.entries).toHaveLength(1);
      expect(logger.entries[0]?.level).toBe('error');
      expect(logger.entries[0]?.logger).toBe('events.handler');
      expect(logger.entries[0]?.message).toBe('message handling failed');
      expect(logger.entries[0]?.error?.message).toBe('handler failed');
      expect(logger.entries[0]?.context?.['event']).toEqual({
        topic: 'test.logging',
        messageId: 'message-2',
        attempt: 1,
        outcome: 'failure',
        durationMs: expect.any(Number),
      });
    },
  );

  // A scope-factory rejection used to take a bypass path OUTSIDE runInContext: no
  // trace id, zero structured fields (topic/messageId interpolated into the
  // message), the error passed as a string, and no duration histogram sample. It
  // now produces the SAME terminal failure record as a handler failure.
  it('emits the terminal failure record when DI scope creation fails', async () => {
    const logger = new MemoryLogger();
    setRootLogger(logger);

    let handlerCalls = 0;
    const definition = handler(TestTopic)
      .options({ timeout: 0 })
      .handle(async () => {});
    const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-3' });
    const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

    await expect(
      dispatchToHandler(
        message,
        ackState,
        abortController,
        envelope,
        definition,
        async () => {
          handlerCalls++;
        },
        { scopeFactory: () => Promise.reject(new Error('scope creation failed')) },
      ),
    ).rejects.toThrow('scope creation failed');

    expect(handlerCalls).toBe(0);
    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    expect(entry?.level).toBe('error');
    expect(entry?.logger).toBe('events.handler');
    expect(entry?.message).toBe('message handling failed');
    // Structured error (with a stack), never error text inside the message.
    expect(entry?.error?.message).toBe('scope creation failed');
    expect(entry?.error?.stack).toBeTruthy();
    expect(entry?.message).not.toContain('scope creation failed');
    // Runs inside the delivery context now, so the record is correlatable — the
    // bypass path emitted it with no trace id at all.
    expect(entry?.traceId).toBe(envelope.traceId ?? envelope.id);
    expect(entry?.traceId).toBeTruthy();
    expect(entry?.context?.['event']).toEqual({
      topic: 'test.logging',
      messageId: 'message-3',
      attempt: 1,
      outcome: 'failure',
      durationMs: expect.any(Number),
    });
  });

  // Same boundary, failure-routed variant: the record is identical whether the
  // transport rethrows or routes the failure to retry/DLQ.
  specTest(
    'routes a scope-creation failure through onFailure with the terminal record intact',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-scope-creation-failure-reaches-the-failure-router-with-its-record',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      const routed: unknown[] = [];
      const definition = handler(TestTopic)
        .options({ timeout: 0 })
        .handle(async () => {});
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-4' });
      const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

      await dispatchToHandler(message, ackState, abortController, envelope, definition, async () => {}, {
        scopeFactory: () => Promise.reject(new Error('scope creation failed')),
        onFailure: (error) => routed.push(error),
      });

      expect(routed).toHaveLength(1);
      expect((routed[0] as Error).message).toBe('scope creation failed');
      expect(logger.entries).toHaveLength(1);
      expect(logger.entries[0]?.message).toBe('message handling failed');
      expect(logger.entries[0]?.context?.['event']).toMatchObject({
        messageId: 'message-4',
        attempt: 1,
        outcome: 'failure',
      });
    },
  );

  // The scope is created inside the terminal try now, so its lifetime must be
  // unchanged: still closed once the delivery ends, failure included.
  specTest(
    'closes the DI scope after a failed delivery',
    {
      feature: 'typescript/event-messaging',
      requirement: 'isolated-invocation',
      check: 'the-scope-is-closed-after-a-failed-delivery',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      let closed = 0;
      const definition = handler(TestTopic)
        .options({ timeout: 0 })
        .handle(async () => {});
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-5' });
      const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

      await dispatchToHandler(
        message,
        ackState,
        abortController,
        envelope,
        definition,
        async () => {
          throw new Error('handler failed');
        },
        {
          scopeFactory: () =>
            Promise.resolve({
              scope: {} as never,
              close: async () => {
                closed++;
              },
            }),
          onFailure: () => {},
        },
      );

      expect(closed).toBe(1);
      expect(logger.entries[0]?.message).toBe('message handling failed');
    },
  );

  // The DI scope is now assigned onto the ambient context from INSIDE
  // runInContext — it used to be assigned before the context existed. That works
  // only because runInContext hands the context object to AsyncLocalStorage by
  // reference, so a later mutation is visible to the running handler. Nothing
  // pinned that wiring (test/handler/handler.test.ts seeds the key directly and
  // never goes through dispatchToHandler), so a snapshotting context would have
  // silently broken useContainer()/resolve() inside every scoped handler.
  specTest(
    'exposes the created DI scope to the handler through the ambient context',
    {
      feature: 'typescript/event-messaging',
      requirement: 'isolated-invocation',
      check: 'the-handler-sees-its-own-injection-scope',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      const scope = { marker: 'scope-under-test' } as never;
      let seen: unknown;

      const definition = handler(TestTopic)
        .options({ timeout: 0 })
        .handle(async () => {});
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID() }, { messageId: 'message-6' });
      const { message, ackState, abortController } = createTransportMessage(envelope, definition.options);

      await dispatchToHandler(
        message,
        ackState,
        abortController,
        envelope,
        definition,
        async () => {
          seen = (useContext() as Record<string | symbol, unknown>)[SCOPE_CONTAINER_KEY];
        },
        { scopeFactory: () => Promise.resolve({ scope, close: async () => {} }) },
      );

      expect(seen).toBe(scope);
      expect(logger.entries[0]?.message).toBe('message handled');
    },
  );
});
