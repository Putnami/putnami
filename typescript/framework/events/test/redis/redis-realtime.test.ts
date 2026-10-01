import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { redisPubSub, redisStream, type RedisCommandClient, type RedisPubSubClient } from '../../src/redis';
import { topic } from '../../src/topic/topic';

const UserUpdated = topic('user.updated', { id: Uuid, name: String });

async function waitUntil(condition: () => boolean, timeoutMs = 250): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

describe('Redis Pub/Sub realtime broker', () => {
  it('publishes live typed messages to subscribers', async () => {
    const redis = new FakePubSubRedis();
    const broker = redisPubSub({ publisher: redis, subscriber: redis, keyPrefix: 'rt' });
    const received: string[] = [];

    const sub = await broker.subscribe(UserUpdated, async (message) => {
      received.push(message.payload.name);
      expect(message.topic).toBe(UserUpdated.name);
      expect(message.attributes.region).toBe('eu');
    });

    await broker.publish(
      UserUpdated,
      { id: crypto.randomUUID(), name: 'Jane' },
      { attributes: { region: 'eu' }, traceId: 'trace-1' },
    );

    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toEqual(['Jane']);
    expect(redis.publishedChannels).toEqual(['rt:user.updated']);
  });

  it('rejects invalid typed payloads before publishing', async () => {
    const redis = new FakePubSubRedis();
    const broker = redisPubSub({ publisher: redis, subscriber: redis });

    // @ts-expect-error Testing runtime validation
    await expect(broker.publish(UserUpdated, { id: 'not-a-uuid', name: 'Jane' })).rejects.toThrow('Invalid payload');
    expect(redis.publishedChannels).toEqual([]);
  });

  it('drops inbound payloads that violate the topic schema (handler not invoked)', async () => {
    const redis = new FakePubSubRedis();
    const broker = redisPubSub({ publisher: redis, subscriber: redis, keyPrefix: 'rt' });
    const received: unknown[] = [];

    const sub = await broker.subscribe(UserUpdated, async (message) => {
      received.push(message.payload);
    });

    // Inject a raw message that bypasses the publisher-side validation,
    // simulating a cross-language producer publishing an off-contract payload.
    const malformed = JSON.stringify({
      id: 'evt-1',
      topic: UserUpdated.name,
      payload: { id: 'not-a-uuid' },
      timestamp: new Date().toISOString(),
      attributes: {},
    });
    await redis.publish('rt:user.updated', malformed);

    // A subsequent valid message must still be delivered.
    await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'Valid' });
    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toHaveLength(1);
    expect((received[0] as { name: string }).name).toBe('Valid');
  });
});

describe('Redis Stream realtime broker', () => {
  it('publishes to Redis streams with bounded retention', async () => {
    const redis = new FakeStreamRedis();
    const broker = redisStream({ client: redis, keyPrefix: 'rt', maxLen: 2, replayTtlMs: 60_000 });

    const first = await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'one' });
    await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'two' });
    const third = await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'three' });

    expect(first.id).toMatch(/^\d+-\d+$/);
    expect(third.id).toMatch(/^\d+-\d+$/);
    expect(redis.stream('rt:user.updated')).toHaveLength(2);
    expect(redis.commands.some((cmd) => cmd.command === 'XTRIM' && cmd.args.includes('MINID'))).toBe(true);
  });

  it('replays from the beginning when requested', async () => {
    const redis = new FakeStreamRedis();
    const broker = redisStream({ client: redis, blockMs: 5 });
    const received: string[] = [];

    await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'before' });

    const sub = await broker.subscribe(
      UserUpdated,
      async (message) => {
        received.push(message.payload.name);
      },
      { from: 'earliest' },
    );

    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toEqual(['before']);
  });

  it('uses stream ids for resume after reconnect', async () => {
    const redis = new FakeStreamRedis();
    const broker = redisStream({ client: redis, blockMs: 5 });
    const first = await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'first' });
    await broker.publish(UserUpdated, { id: crypto.randomUUID(), name: 'second' });

    const received: string[] = [];
    const sub = await broker.subscribe(
      UserUpdated,
      async (message) => {
        received.push(message.payload.name);
      },
      { from: first.id },
    );

    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toEqual(['second']);
  });

  it('does not replay existing entries when subscribing from latest', async () => {
    const redis = new FakeStreamRedis();
    const broker = redisStream({ client: redis, blockMs: 5 });
    const received: string[] = [];

    await broker.publish('live.topic', { name: 'before' });
    const sub = await broker.subscribe<{ name: string }>('live.topic', async (message) => {
      received.push(message.payload.name);
    });

    await Bun.sleep(10);
    expect(received).toEqual([]);

    await broker.publish('live.topic', { name: 'after' });
    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toEqual(['after']);
  });
});

class FakePubSubRedis implements RedisPubSubClient {
  readonly publishedChannels: string[] = [];
  private readonly handlers = new Map<string, Set<(message: string, channel: string) => void>>();

  async publish(channel: string, message: string): Promise<void> {
    this.publishedChannels.push(channel);
    for (const handler of this.handlers.get(channel) ?? []) {
      handler(message, channel);
    }
  }

  async subscribe(channel: string, handler: (message: string, channel: string) => void): Promise<void> {
    const handlers = this.handlers.get(channel) ?? new Set();
    handlers.add(handler);
    this.handlers.set(channel, handlers);
  }

  async unsubscribe(channel: string, handler?: (message: string, channel: string) => void): Promise<void> {
    if (!handler) {
      this.handlers.delete(channel);
      return;
    }

    const handlers = this.handlers.get(channel);
    handlers?.delete(handler);
  }
}

class FakeStreamRedis implements RedisCommandClient {
  readonly commands: Array<{ command: string; args: string[] }> = [];
  private readonly streams = new Map<string, [string, string[]][]>();
  private readonly waiters = new Map<string, Set<() => void>>();
  private sequence = 0;

  stream(key: string): [string, string[]][] {
    return this.streams.get(key) ?? [];
  }

  async command(command: string, args: string[]): Promise<unknown> {
    this.commands.push({ command, args });
    switch (command) {
      case 'XADD':
        return this.xadd(args);
      case 'XREAD':
        return await this.xread(args);
      case 'XTRIM':
        return this.xtrim(args);
      default:
        throw new Error(`Unsupported Redis command ${command}`);
    }
  }

  private xadd(args: string[]): string {
    const key = args[0];
    let index = 1;
    let maxLen: number | undefined;
    if (args[index] === 'MAXLEN') {
      maxLen = Number(args[index + 2]);
      index += 3;
    }

    // Skip Redis-generated id marker.
    index++;

    const id = `${Date.now()}-${++this.sequence}`;
    const entry: [string, string[]] = [id, args.slice(index)];
    const entries = this.streams.get(key) ?? [];
    entries.push(entry);
    if (maxLen && entries.length > maxLen) {
      entries.splice(0, entries.length - maxLen);
    }
    this.streams.set(key, entries);
    this.resolveWaiters(key);
    return id;
  }

  private async xread(args: string[]): Promise<unknown> {
    const blockIndex = args.indexOf('BLOCK');
    const streamIndex = args.indexOf('STREAMS');
    const blockMs = blockIndex >= 0 ? Number(args[blockIndex + 1]) : 0;
    const key = args[streamIndex + 1];
    const requestedId = args[streamIndex + 2];
    const lastId = requestedId === '$' ? this.lastId(key) : requestedId;

    const read = () => this.formatRead(key, lastId);
    const immediate = read();
    if (immediate.length > 0 || blockMs <= 0) {
      return immediate;
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

  private xtrim(args: string[]): number {
    const key = args[0];
    const minIdIndex = args.indexOf('MINID');
    if (minIdIndex < 0) {
      return 0;
    }

    const cutoff = args[minIdIndex + 2];
    const entries = this.stream(key);
    const next = entries.filter(([id]) => compareStreamIds(id, cutoff) >= 0);
    this.streams.set(key, next);
    return entries.length - next.length;
  }

  private formatRead(key: string, lastId: string): unknown[] {
    const entries = this.stream(key).filter(([id]) => compareStreamIds(id, lastId) > 0);
    return entries.length > 0 ? [[key, entries]] : [];
  }

  private lastId(key: string): string {
    const entries = this.stream(key);
    return entries.at(-1)?.[0] ?? '0-0';
  }

  private resolveWaiters(key: string): void {
    for (const notify of this.waiters.get(key) ?? []) {
      notify();
    }
  }
}

function compareStreamIds(left: string, right: string): number {
  const [leftMs = '0', leftSeq = '0'] = left.split('-');
  const [rightMs = '0', rightSeq = '0'] = right.split('-');
  const ms = Number(leftMs) - Number(rightMs);
  return ms === 0 ? Number(leftSeq) - Number(rightSeq) : ms;
}
