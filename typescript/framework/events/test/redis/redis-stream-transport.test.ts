import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { handler } from '../../src/handler/handler';
import { redisStreamTransport, type RedisCommandClient } from '../../src/redis';
import { buildEnvelope } from '../../src/transport/transport';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('redis.transport.event', { id: Uuid, value: String });

async function waitUntil(condition: () => boolean, timeoutMs = 250): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

describe('RedisStreamTransport', () => {
  it('delivers and acknowledges stream messages', async () => {
    const redis = new FakeReliableStreamRedis();
    const transport = redisStreamTransport({ client: redis, blockMs: 5, consumerName: 'consumer-1' });
    const received: string[] = [];

    const def = handler(TestTopic)
      .options({ group: 'orders-worker', timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async (message) => {
      received.push((message.payload as { value: string }).value);
    });
    await transport.start();

    await transport.publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'ok' }));
    await waitUntil(() => received.length === 1);
    await transport.stop();

    expect(received).toEqual(['ok']);
    expect(redis.acks).toEqual([{ stream: 'redis.transport.event', group: 'orders-worker' }]);
  });

  it('replays all pending entries before reading new messages', async () => {
    const redis = new FakeReliableStreamRedis();
    const transport = redisStreamTransport({
      client: redis,
      blockMs: 5,
      count: 2,
      consumerName: 'consumer-1',
    });
    const received: string[] = [];

    const def = handler(TestTopic)
      .options({ group: 'orders-worker', timeout: 0 })
      .handle(async () => {});
    redis.seedPending(
      'redis.transport.event',
      'orders-worker',
      'consumer-1',
      ['one', 'two', 'three'].map((value) =>
        buildEnvelope(TestTopic, { id: crypto.randomUUID(), value }, { messageId: `pending-${value}` }),
      ),
    );

    await transport.subscribe(def, async (message) => {
      received.push((message.payload as { value: string }).value);
    });
    await transport.start();

    await waitUntil(() => received.length === 3);
    await transport.stop();

    expect(received).toEqual(['one', 'two', 'three']);
    expect(redis.acks).toHaveLength(3);
  });

  it('routes exhausted failures to the DLQ stream', async () => {
    const redis = new FakeReliableStreamRedis();
    const transport = redisStreamTransport({ client: redis, blockMs: 5, consumerName: 'consumer-1' });

    const def = handler(TestTopic)
      .options({ group: 'orders-worker', maxRetries: 1, timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async () => {
      throw new Error('boom');
    });
    await transport.start();

    await transport.publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'fail' }));
    await waitUntil(() => redis.stream('redis.transport.event.dlq').length === 1);
    await transport.stop();

    const dlq = JSON.parse(fieldValue(redis.stream('redis.transport.event.dlq')[0][1], 'message') ?? '{}') as {
      topic: string;
      attributes: Record<string, string>;
    };
    expect(dlq.topic).toBe('redis.transport.event.dlq');
    expect(dlq.attributes['dlq.original_topic']).toBe('redis.transport.event');
    expect(dlq.attributes['dlq.error']).toBe('boom');
  });

  it('scopes retries to the failing broadcast group without redelivering to others', async () => {
    // Regression guard for the cross-group amplification bug: with two broadcast
    // handlers on the same topic (⇒ two consumer groups reading the shared topic
    // stream), a retry from the group whose handler FAILED must be re-consumed
    // only by that group. The other group, which already succeeded on first
    // delivery, must NOT be re-invoked by the retry. The old code re-XADD'd the
    // retry to the shared topic stream, fanning it out to every group's '>'
    // cursor.
    const redis = new FakeReliableStreamRedis();
    const transport = redisStreamTransport({ client: redis, blockMs: 5, consumerName: 'consumer-1' });

    let invokedA = 0;
    let invokedB = 0;
    let failFirst = true;

    // Distinct handler bodies ⇒ distinct broadcast identities ⇒ distinct groups.
    // The bodies must differ in real code (not just comments): the runtime
    // toString() strips comments, so identical-looking arrows would collapse to
    // one broadcast group.
    const defA = handler(TestTopic)
      .options({ distribution: 'broadcast', maxRetries: 3, maxBackoff: 1, timeout: 0 })
      .handle(async () => {
        invokedA += 0;
      });
    const defB = handler(TestTopic)
      .options({ distribution: 'broadcast', maxRetries: 3, maxBackoff: 1, timeout: 0 })
      .handle(async () => {
        invokedB += 0;
      });

    await transport.subscribe(defA, async () => {
      invokedA += 1;
      if (failFirst) {
        failFirst = false;
        throw new Error('A transient failure');
      }
    });
    await transport.subscribe(defB, async () => {
      invokedB += 1;
    });
    await transport.start();

    await transport.publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'x' }));

    // A retries once (fail → succeed); B succeeds once and stays at one.
    await waitUntil(() => invokedA >= 2 && invokedB >= 1);
    // Give any erroneous cross-group redelivery a chance to surface before asserting.
    await Bun.sleep(20);
    await transport.stop();

    expect(invokedA).toBe(2);
    // The core assertion: B saw the event exactly once — A's retry never leaked
    // into B's group.
    expect(invokedB).toBe(1);
    // The retry landed on a per-group retry stream, never on B's stream. No
    // stream other than A's own retry stream should have received the retry.
    const retryStreams = [...redis.streamKeys()].filter((key) => key.endsWith(':retry'));
    expect(retryStreams.length).toBe(1);
    expect(redis.stream(retryStreams[0] ?? '').length).toBe(1);
  });

  it('routes scope factory failures through retry and DLQ handling', async () => {
    const redis = new FakeReliableStreamRedis();
    const transport = redisStreamTransport({ client: redis, blockMs: 5, consumerName: 'consumer-1' });

    const def = handler(TestTopic)
      .options({ group: 'orders-worker', maxRetries: 1, timeout: 0 })
      .handle(async () => {});
    transport.setScopeFactory(async () => {
      throw new Error('scope failed');
    });
    await transport.subscribe(def, async () => {});
    await transport.start();

    await transport.publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'fail' }));
    await waitUntil(() => redis.stream('redis.transport.event.dlq').length === 1);
    await transport.stop();

    const dlq = JSON.parse(fieldValue(redis.stream('redis.transport.event.dlq')[0][1], 'message') ?? '{}') as {
      attributes: Record<string, string>;
    };
    expect(dlq.attributes['dlq.error']).toBe('scope failed');
    expect(redis.acks).toContainEqual({ stream: 'redis.transport.event', group: 'orders-worker' });
  });
});

class FakeReliableStreamRedis implements RedisCommandClient {
  readonly acks: Array<{ stream: string; group: string }> = [];
  private readonly streams = new Map<string, [string, string[]][]>();
  private readonly delivered = new Map<string, Set<string>>();
  private readonly pending = new Map<string, Set<string>>();
  private readonly waiters = new Map<string, Set<() => void>>();
  private sequence = 0;

  stream(key: string): [string, string[]][] {
    return this.streams.get(key) ?? [];
  }

  streamKeys(): string[] {
    return [...this.streams.keys()];
  }

  seedPending(stream: string, group: string, consumer: string, envelopes: unknown[]): void {
    const entries = envelopes.map((envelope) => {
      const id = `${Date.now()}-${++this.sequence}`;
      return [id, ['message', JSON.stringify(envelope)]] as [string, string[]];
    });
    this.streams.set(stream, [...this.stream(stream), ...entries]);

    const deliveredKey = `${stream}:${group}`;
    const delivered = this.delivered.get(deliveredKey) ?? new Set<string>();
    const pendingKey = `${stream}:${group}:${consumer}`;
    const pending = this.pending.get(pendingKey) ?? new Set<string>();
    for (const [entryId] of entries) {
      delivered.add(entryId);
      pending.add(entryId);
    }
    this.delivered.set(deliveredKey, delivered);
    this.pending.set(pendingKey, pending);
  }

  async command(command: string, args: string[]): Promise<unknown> {
    switch (command) {
      case 'XGROUP':
        return 'OK';
      case 'XADD':
        return this.xadd(args);
      case 'XREADGROUP':
        return await this.xreadgroup(args);
      case 'XACK':
        this.acks.push({ stream: args[0], group: args[1] });
        this.removePending(args[0], args[1], args[2]);
        return 1;
      default:
        throw new Error(`Unsupported Redis command ${command}`);
    }
  }

  private xadd(args: string[]): string {
    const key = args[0];
    let index = 1;
    if (args[index] === 'MAXLEN') {
      index += 3;
    }
    index++;

    const id = `${Date.now()}-${++this.sequence}`;
    const entries = this.streams.get(key) ?? [];
    entries.push([id, args.slice(index)]);
    this.streams.set(key, entries);
    for (const waiter of this.waiters.get(key) ?? []) {
      waiter();
    }
    return id;
  }

  private async xreadgroup(args: string[]): Promise<unknown> {
    const group = args[1];
    const consumer = args[2];
    const blockIndex = args.indexOf('BLOCK');
    const blockMs = blockIndex >= 0 ? Number(args[blockIndex + 1]) : 0;
    const count = Number(args[args.indexOf('COUNT') + 1]);
    const streamIndex = args.indexOf('STREAMS');
    const key = args[streamIndex + 1];
    const id = args[streamIndex + 2];
    const read = () => this.formatRead(key, group, consumer, id, count);
    const immediate = read();
    if (immediate.length > 0) {
      return immediate;
    }
    // Non-blocking read (no BLOCK arg, e.g. the retry stream): empty batch now.
    if (blockMs <= 0) {
      return [];
    }

    return await new Promise<unknown>((resolve) => {
      const timeout = setTimeout(() => {
        this.waiters.get(key)?.delete(notify);
        resolve([]);
      }, blockMs);
      const notify = () => {
        clearTimeout(timeout);
        this.waiters.get(key)?.delete(notify);
        resolve(read());
      };
      const waiters = this.waiters.get(key) ?? new Set();
      waiters.add(notify);
      this.waiters.set(key, waiters);
    });
  }

  private formatRead(key: string, group: string, consumer: string, id: string, count: number): unknown[] {
    if (id !== '>') {
      const pending = this.pending.get(`${key}:${group}:${consumer}`) ?? new Set<string>();
      const entries = this.stream(key)
        .filter(([entryId]) => pending.has(entryId) && compareStreamIds(entryId, id) > 0)
        .slice(0, count);
      return entries.length > 0 ? [[key, entries]] : [];
    }

    const deliveredKey = `${key}:${group}`;
    const delivered = this.delivered.get(deliveredKey) ?? new Set<string>();
    const entries = this.stream(key)
      .filter(([entryId]) => !delivered.has(entryId))
      .slice(0, count);
    const pendingKey = `${key}:${group}:${consumer}`;
    const pending = this.pending.get(pendingKey) ?? new Set<string>();
    for (const [entryId] of entries) {
      delivered.add(entryId);
      pending.add(entryId);
    }
    this.delivered.set(deliveredKey, delivered);
    this.pending.set(pendingKey, pending);
    return entries.length > 0 ? [[key, entries]] : [];
  }

  private removePending(stream: string, group: string, entryId: string): void {
    for (const [key, pending] of this.pending) {
      if (key.startsWith(`${stream}:${group}:`)) {
        pending.delete(entryId);
      }
    }
  }
}

function fieldValue(fields: unknown[], name: string): string | undefined {
  for (let i = 0; i < fields.length; i += 2) {
    if (String(fields[i]) === name) {
      return String(fields[i + 1]);
    }
  }
  return undefined;
}

function compareStreamIds(left: string, right: string): number {
  const [leftMs = '0', leftSeq = '0'] = left.split('-');
  const [rightMs = '0', rightSeq = '0'] = right.split('-');
  const ms = Number(leftMs) - Number(rightMs);
  if (ms !== 0) {
    return ms;
  }
  return Number(leftSeq) - Number(rightSeq);
}
