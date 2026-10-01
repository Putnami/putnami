import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { type Context, resetDefaultLogger, runInContext, setRootLogger, Uuid } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { clearTransport, getPublisher, setTransport } from '../../src/publisher/publisher';
import { topic } from '../../src/topic/topic';
import type { Transport } from '../../src/transport/transport';

const Orders = topic('logging.publish.orders', { id: Uuid });

function createMockTransport(): Transport & { published: unknown[] } {
  const published: unknown[] = [];
  return {
    published,
    async publish(_topic: string, envelope: unknown) {
      published.push(envelope);
    },
    async subscribe() {},
    async start() {},
    async stop() {},
  };
}

function eventGroupOf(entry: { data?: unknown[]; context?: Record<string, unknown> }): Record<string, unknown> {
  // The JSON sink flattens a lone object param over the entry context, so a
  // param-supplied group wins when both are present.
  const data = entry.data?.[0] as Record<string, unknown> | undefined;
  const fromData = data?.['event'];
  if (fromData && typeof fromData === 'object') {
    return fromData as Record<string, unknown>;
  }
  return (entry.context?.['event'] ?? {}) as Record<string, unknown>;
}

describe('publish logging', () => {
  let logger: MemoryLogger;

  beforeEach(() => {
    clearTransport();
    logger = new MemoryLogger();
    setRootLogger(logger);
  });

  afterEach(() => {
    clearTransport();
    resetDefaultLogger();
  });

  it('emits the publish record under the pinned logger name with structured fields', async () => {
    setTransport(createMockTransport());

    await getPublisher(Orders)({ id: crypto.randomUUID() });

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    expect(entry?.level).toBe('debug');
    // Pinned logger name of the contract: dots, never colons.
    expect(entry?.logger).toBe('events.publish');
    expect(entry?.message).toBe('message published');
    // Detached (no request scope), the record still carries its own fields: the
    // pre-fix code dropped the `logger.with(...)` return, so the fields vanished.
    expect(eventGroupOf(entry ?? {})).toEqual({
      topic: 'logging.publish.orders',
      messageId: expect.any(String),
    });
  });

  it('accumulates repeated publishes on the caller boundary instead of last-write-losing', async () => {
    setTransport(createMockTransport());
    const publish = getPublisher(Orders);

    const context: Context = { traceId: 'trace-publish' };
    await runInContext(context, async () => {
      await publish({ id: crypto.randomUUID() });
      await publish({ id: crypto.randomUUID() });
    });

    const group = (context.logContext?.['event'] ?? {}) as Record<string, unknown>;
    // Two publishes in one boundary: an appended list plus the paired counter,
    // both visible on that boundary's terminal record (the HTTP middleware's).
    expect(group['publishCount']).toBe(2);
    expect(group['publishes']).toEqual([
      { topic: 'logging.publish.orders', messageId: expect.any(String) },
      { topic: 'logging.publish.orders', messageId: expect.any(String) },
    ]);
    // The two message ids are distinct — the second publish did not overwrite the
    // first, which is what `with()` used to do inside a request.
    const publishes = group['publishes'] as { messageId: string }[];
    expect(publishes[0]?.messageId).not.toBe(publishes[1]?.messageId);
    expect(logger.entries).toHaveLength(2);
  });

  it('accumulates nothing when the transport rejects the publish', async () => {
    setTransport({
      async publish() {
        throw new Error('transport down');
      },
      async subscribe() {},
      async start() {},
      async stop() {},
    });

    const context: Context = { traceId: 'trace-publish-failure' };
    await runInContext(context, async () => {
      await expect(getPublisher(Orders)({ id: crypto.randomUUID() })).rejects.toThrow('transport down');
    });

    // Go parity: the publish fields are accumulated only AFTER the transport
    // accepted the message, so a failed publish never claims one.
    expect(context.logContext?.['event']).toBeUndefined();
    expect(logger.entries).toHaveLength(0);
  });
});
