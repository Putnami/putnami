import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type LogEntry, resetDefaultLogger, setRootLogger, Uuid } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { handler } from '../../src/handler/handler';
import type { Subscription } from '../../src/server/delivery-queue';
import { FailureRouter, type FailureRouterHost } from '../../src/server/failure-router';
import { MemoryBroker } from '../../src/server/memory-broker';
import type { Message } from '../../src/topic/message';
import { topic } from '../../src/topic/topic';
import { buildEnvelope, type Envelope } from '../../src/transport/transport';

// These tests pin the event boundary's broker records (protocols/logging/
// conformance) by driving the REAL memory broker and reading the records back
// through a memory sink — the log helpers are never called directly, so a broker
// path that stops routing through them fails here. They are the TypeScript twin
// of go/framework/events/delivery_logging_test.go.

const HANDLER_LOGGER = 'events.handler';
const BROKER_LOGGER = 'events.broker';

const FailingTopic = topic('logging.broker.orders', { id: Uuid });
const DlqTopic = topic('logging.broker.orders.dlq', { id: Uuid });
const HANDLER_ERROR = 'conformance handler failure';

async function waitUntil(condition: () => boolean, timeoutMs = 2000): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

/**
 * The event group as the sink renders it: a lone object param is flattened over
 * the entry context, so a param-supplied group wins over the delivery's
 * accumulated one (the same precedence as Go's closed attr over its field bag).
 */
function eventGroupOf(entry: LogEntry): Record<string, unknown> {
  const data = entry.data?.[0] as Record<string, unknown> | undefined;
  const fromData = data?.['event'];
  if (fromData && typeof fromData === 'object') {
    return fromData as Record<string, unknown>;
  }
  return (entry.context?.['event'] ?? {}) as Record<string, unknown>;
}

function recordsOf(logger: MemoryLogger, loggerName: string, message: string): LogEntry[] {
  return logger.entries.filter((entry) => entry.logger === loggerName && entry.message === message);
}

function oneRecordOf(logger: MemoryLogger, loggerName: string, message: string): LogEntry {
  const found = recordsOf(logger, loggerName, message);
  const all = logger.entries.map((e) => `[${e.level}] ${e.logger} ${e.message}`).join('\n  ');
  expect(found.length, `expected exactly 1 "${message}" from ${loggerName}, got:\n  ${all}`).toBe(1);
  return found[0] as LogEntry;
}

function envelopeFor(topicDef: typeof FailingTopic, messageId: string): Envelope {
  return buildEnvelope(topicDef, { id: crypto.randomUUID() }, { messageId });
}

describe('broker failure logging', () => {
  let broker: MemoryBroker | undefined;

  afterEach(async () => {
    await broker?.stop();
    broker = undefined;
    resetDefaultLogger();
  });

  specTest(
    'emits a structured retry record and one dead-letter record once the DLQ accepts it',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'one-dead-letter-record-is-emitted-once-the-dlq-accepts-it',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);
      broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();

      let attempts = 0;
      let deadLettered: Message<unknown> | undefined;
      const failing = handler(FailingTopic)
        .options({ maxRetries: 2, maxBackoff: 1, timeout: 0, dlq: true })
        .handle(async () => {});
      await broker.subscribe(failing, async () => {
        attempts++;
        throw new Error(HANDLER_ERROR);
      });
      await broker.subscribe(
        handler(DlqTopic)
          .options({ timeout: 0 })
          .handle(async () => {}),
        async (msg) => {
          deadLettered = msg;
        },
      );

      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-retry'));
      await waitUntil(() => deadLettered !== undefined);

      expect(attempts).toBe(2);

      // Retry record: WARNING (the failure is recoverable), the FAILED delivery's
      // attempt plus the scheduled nextAttempt, and the structured error.
      const retry = oneRecordOf(logger, BROKER_LOGGER, 'message retry scheduled');
      expect(retry.level).toBe('warn');
      expect(retry.error?.message).toBe(HANDLER_ERROR);
      expect(retry.message).not.toContain(HANDLER_ERROR);
      expect(eventGroupOf(retry)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-retry',
        attempt: 1,
        nextAttempt: 2,
        delayMs: expect.any(Number),
        maxRetries: 2,
        outcome: 'failure',
      });

      // Dead-letter record: ERROR, exactly once, reporting the TRUE final attempt
      // (never attempt + 1) and the DLQ topic the message was routed to.
      const dlq = oneRecordOf(logger, BROKER_LOGGER, 'message dead-lettered');
      expect(dlq.level).toBe('error');
      expect(dlq.error?.message).toBe(HANDLER_ERROR);
      expect(eventGroupOf(dlq)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-retry',
        attempt: 2,
        dlqTopic: 'logging.broker.orders.dlq',
        outcome: 'failure',
      });

      // Terminal records stay one per delivery, independent of the broker records.
      expect(recordsOf(logger, HANDLER_LOGGER, 'message handling failed')).toHaveLength(2);
      // No drop was claimed: the dead-letter was accepted.
      expect(recordsOf(logger, BROKER_LOGGER, 'message dropped')).toHaveLength(0);
    },
  );

  specTest(
    'emits a drop record with the true final attempt when retries are exhausted without a DLQ',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'a-drop-record-carries-the-true-final-attempt',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);
      broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();

      let attempts = 0;
      const failing = handler(FailingTopic)
        .options({ maxRetries: 2, maxBackoff: 1, timeout: 0, dlq: false })
        .handle(async () => {});
      await broker.subscribe(failing, async () => {
        attempts++;
        throw new Error(HANDLER_ERROR);
      });

      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-drop'));
      await waitUntil(() => recordsOf(logger, BROKER_LOGGER, 'message dropped').length === 1);

      expect(attempts).toBe(2);
      const dropped = oneRecordOf(logger, BROKER_LOGGER, 'message dropped');
      expect(dropped.level).toBe('error');
      expect(dropped.error?.message).toBe(HANDLER_ERROR);
      expect(eventGroupOf(dropped)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-drop',
        attempt: 2,
        reason: 'retries_exhausted',
        outcome: 'failure',
      });
      expect(recordsOf(logger, BROKER_LOGGER, 'message dead-lettered')).toHaveLength(0);
    },
  );

  specTest(
    'reports a dead-letter with no DLQ subscriber as a drop, not a dead-letter',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'a-dead-letter-nobody-subscribes-to-is-recorded-as-a-drop',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);
      broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();

      const failing = handler(FailingTopic)
        .options({ maxRetries: 1, timeout: 0, dlq: true })
        .handle(async () => {});
      await broker.subscribe(failing, async () => {
        throw new Error(HANDLER_ERROR);
      });

      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-no-subscriber'));
      await waitUntil(() => recordsOf(logger, BROKER_LOGGER, 'message dropped').length === 1);

      // Nothing will ever receive the message, so an operator must not be pointed
      // at a DLQ it never reached.
      expect(recordsOf(logger, BROKER_LOGGER, 'message dead-lettered')).toHaveLength(0);
      const dropped = oneRecordOf(logger, BROKER_LOGGER, 'message dropped');
      expect(dropped.level).toBe('error');
      expect(dropped.error?.message).toBe(HANDLER_ERROR);
      expect(eventGroupOf(dropped)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-no-subscriber',
        attempt: 1,
        reason: 'dlq_no_subscriber',
        dlqTopic: 'logging.broker.orders.dlq',
        outcome: 'failure',
      });
      // The envelope itself is still retrievable from the bounded buffer.
      const captured = broker.getDeadLetters();
      expect(captured).toHaveLength(1);
      expect(captured[0]?.error).toBe(HANDLER_ERROR);
      expect(captured[0]?.envelope.id).toBe('m-no-subscriber');
    },
  );

  // A re-delivery the queue refuses is a `retry_enqueue_failed` drop — reporting
  // `retries_exhausted` would blame a limit that was never reached (attempt 2 of
  // 3 here). Driven through the real FailureRouter with a refusing host.
  specTest(
    'reports a refused retry re-enqueue as retry_enqueue_failed',
    {
      feature: 'typescript/event-messaging',
      requirement: 'residual-failures-are-routed',
      check: 'a-routing-failure-is-logged-as-its-own-record',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);

      const definition = handler(FailingTopic)
        .options({ maxRetries: 3, maxBackoff: 1, timeout: 0, dlq: false })
        .handle(async () => {});
      const subscription: Subscription = { definition, callback: async () => {} };
      const host: FailureRouterHost = {
        enqueue: () => {
          throw new Error('Queue limit reached');
        },
        selectTargets: (_topic, subs) => subs,
        getSubscriptions: () => undefined,
        isRunning: () => true,
      };
      const router = new FailureRouter(host, 10);

      router.handleFailure(new Error(HANDLER_ERROR), envelopeFor(FailingTopic, 'm-retry-enqueue'), subscription);
      try {
        await waitUntil(() => recordsOf(logger, BROKER_LOGGER, 'message dropped').length === 1);
      } finally {
        router.cancelRetries();
      }

      const dropped = oneRecordOf(logger, BROKER_LOGGER, 'message dropped');
      expect(dropped.error?.message).toBe('Queue limit reached');
      expect(eventGroupOf(dropped)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-retry-enqueue',
        attempt: 2,
        reason: 'retry_enqueue_failed',
        outcome: 'failure',
      });
    },
  );

  specTest(
    'emits a queue-full drop record with the queue limit and no handler error',
    {
      feature: 'typescript/event-messaging',
      requirement: 'one-record-per-message',
      check: 'a-queue-full-drop-records-the-limit-and-no-handler-error',
    },
    async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);
      broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();

      let release = () => {};
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      const slow = handler(FailingTopic)
        .options({ concurrency: 1, queueLimit: 1, overflow: 'drop', timeout: 0 })
        .handle(async () => {});
      await broker.subscribe(slow, async () => {
        await gate;
      });

      // 1 running + 1 queued + 1 over the limit → the third is dropped.
      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-running'));
      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-queued'));
      await broker.publish(FailingTopic.name, envelopeFor(FailingTopic, 'm-dropped'));

      const dropped = oneRecordOf(logger, BROKER_LOGGER, 'message dropped');
      // Drops are ERROR — the message is lost (it used to report at WARNING).
      expect(dropped.level).toBe('error');
      // The delivery never ran, so there is no handler error to attach.
      expect(dropped.error).toBeUndefined();
      expect(eventGroupOf(dropped)).toEqual({
        topic: 'logging.broker.orders',
        messageId: 'm-dropped',
        attempt: 1,
        reason: 'queue_full',
        queueLimit: 1,
        outcome: 'failure',
      });

      release();
    },
  );
});
