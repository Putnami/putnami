import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { application, http, HttpResponse, logger as loggerPlugin, trace } from '@putnami/application';
import type { Application } from '@putnami/application';
import { resetDefaultLogger, setRootLogger, Uuid } from '@putnami/runtime';
import { assertRecord, findCase, findRecord, loadCases, MemoryLogger } from '@putnami/runtime/testing';
import { handler } from '../src/handler/handler';
import { clearTransport, getPublisher, setTransport } from '../src/publisher/publisher';
import { MemoryBroker } from '../src/server/memory-broker';
import { topic } from '../src/topic/topic';
import { buildEnvelope, type Envelope } from '../src/transport/transport';

/**
 * This file executes the event boundary's cases of the canonical cross-runtime log
 * corpus (`protocols/logging/conformance`) against the records the REAL memory
 * broker emits through the REAL JSON sink path (`buildJsonRecord`). Its Go twin is
 * `go/framework/events/logging_cross_language_test.go`.
 *
 * It also owns the corpus's accumulation case
 * (`http.terminal.success-with-publishes`): publishing is what accumulates onto an
 * HTTP terminal record, and this package is the one that depends on
 * `@putnami/application` (never the reverse), so the only place both halves of
 * that case exist is here.
 */

const eventCases = loadCases('event');
const httpCases = loadCases('http');

/** The corpus's fixture topic; its dead-letter topic is the same name + `.dlq`. */
const Orders = topic('logging.conformance.orders', { id: Uuid });
const OrdersDlq = topic('logging.conformance.orders.dlq', { id: Uuid });

/** The corpus pins this exact failure text on every event failure record. */
const HANDLER_ERROR = 'conformance handler failure';

let broker: MemoryBroker | undefined;
let app: Application | undefined;

afterEach(async () => {
  await broker?.stop();
  broker = undefined;
  await app?.stop();
  app = undefined;
  clearTransport();
  resetDefaultLogger();
});

async function waitUntil(condition: () => boolean, timeoutMs = 2000): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

/** Start a real memory broker whose records land in a fresh MemoryLogger. */
async function recordingBroker(): Promise<MemoryLogger> {
  const memory = new MemoryLogger();
  setRootLogger(memory);
  broker = new MemoryBroker({ simulateDuplicates: false });
  await broker.start();
  return memory;
}

function envelopeFor(messageId: string): Envelope {
  return buildEnvelope(Orders, { id: crypto.randomUUID() }, { messageId });
}

describe('event logging conformance', () => {
  specTest(
    'emits the corpus terminal record for a handled delivery',
    {
      feature: 'typescript/event-messaging',
      requirement: 'cross-language-records',
      check: 'a-handled-delivery-emits-the-corpus-terminal-record',
    },
    async () => {
      const want = findCase(eventCases, 'event.terminal.success');
      const memory = await recordingBroker();

      let handled = false;
      await broker?.subscribe(
        handler(Orders)
          .options({ timeout: 0 })
          .handle(async () => {}),
        async () => {
          handled = true;
        },
      );
      await broker?.publish(Orders.name, envelopeFor('m-success'));
      await waitUntil(() => handled);

      assertRecord(findRecord(memory.entries, want), want);
    },
  );

  specTest(
    'emits the corpus terminal record for a failed delivery',
    {
      feature: 'typescript/event-messaging',
      requirement: 'cross-language-records',
      check: 'a-failed-delivery-emits-the-corpus-terminal-record',
    },
    async () => {
      const want = findCase(eventCases, 'event.terminal.failure');
      const memory = await recordingBroker();

      // maxRetries 1 = one total delivery attempt: no retry, no DLQ.
      await broker?.subscribe(
        handler(Orders)
          .options({ maxRetries: 1, timeout: 0, dlq: false })
          .handle(async () => {}),
        async () => {
          throw new Error(HANDLER_ERROR);
        },
      );
      await broker?.publish(Orders.name, envelopeFor('m-failure'));
      await waitUntil(() => memory.entries.some((e) => e.message === 'message handling failed'));

      const terminal = findRecord(memory.entries, want);
      assertRecord(terminal, want);
      expect(terminal.message).not.toContain(HANDLER_ERROR);
    },
  );

  // The retry and dead-letter cases share one drive block in the corpus (maxRetries
  // 2 + a subscribed DLQ), so they are asserted from one delivery sequence: attempt
  // 1 emits the retry record with nextAttempt 2, attempt 2 emits the dead-letter
  // with the TRUE final attempt 2. The dead-letter is emitted only once the DLQ has
  // accepted it, and never alongside a drop.
  specTest(
    'emits the corpus retry and dead-letter records for an exhausted delivery',
    {
      feature: 'typescript/event-messaging',
      requirement: 'cross-language-records',
      check: 'an-exhausted-delivery-emits-the-corpus-retry-and-dead-letter-records',
    },
    async () => {
      const wantRetry = findCase(eventCases, 'event.terminal.retry');
      const wantDlq = findCase(eventCases, 'event.terminal.dlq');
      const memory = await recordingBroker();

      let attempts = 0;
      let deadLettered = false;
      await broker?.subscribe(
        handler(Orders)
          .options({ maxRetries: 2, maxBackoff: 1, timeout: 0, dlq: true })
          .handle(async () => {}),
        async () => {
          attempts++;
          throw new Error(HANDLER_ERROR);
        },
      );
      await broker?.subscribe(
        handler(OrdersDlq)
          .options({ timeout: 0 })
          .handle(async () => {}),
        async () => {
          deadLettered = true;
        },
      );

      await broker?.publish(Orders.name, envelopeFor('m-dlq'));
      await waitUntil(() => deadLettered);
      expect(attempts).toBe(2);

      assertRecord(findRecord(memory.entries, wantRetry), wantRetry);
      assertRecord(findRecord(memory.entries, wantDlq), wantDlq);
      expect(memory.entries.filter((e) => e.message === 'message dropped')).toHaveLength(0);
    },
  );
});

describe('http logging conformance (accumulation)', () => {
  it('summarizes two publishes on the request terminal record', async () => {
    const want = findCase(httpCases, 'http.terminal.success-with-publishes');

    const memory = new MemoryLogger();
    setRootLogger(memory);
    broker = new MemoryBroker({ simulateDuplicates: false });
    await broker.start();
    setTransport(broker);

    const publish = getPublisher(Orders);
    const plugin = http({ port: 0 });
    plugin.post('/conformance/orders', async () => {
      // Two publishes inside ONE request must ACCUMULATE on that request's single
      // terminal record (appended list + incremented counter), not last-write-win.
      await publish({ id: crypto.randomUUID() });
      await publish({ id: crypto.randomUUID() });
      return HttpResponse.json({ ok: true });
    });
    app = application().use(plugin).use(trace()).use(loggerPlugin());
    await app.start();

    const res = await fetch(`http://localhost:${plugin.getServer()?.port}/conformance/orders`, { method: 'POST' });
    await res.text();

    assertRecord(findRecord(memory.entries, want), want);
  });
});
