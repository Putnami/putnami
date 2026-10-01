import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import type { TopicDefinition } from '../topic/topic';
import {
  createBunRedisClient,
  toRedisCommandClient,
  type RedisCommandClient,
  type RedisCommandSource,
} from './bun-redis';
import {
  createRealtimeMessage,
  decodeRealtimeMessage,
  getRealtimePayloadError,
  getRealtimeTopicName,
  jsonRealtimeCodec,
  resolveRealtimeKey,
  type RealtimeBroker,
  type RealtimeCodec,
  type RealtimeHandler,
  type RealtimeMessage,
  type RealtimePublishOptions,
  type RealtimeSubscribeOptions,
  type RealtimeSubscription,
  type RealtimeTopic,
  validateRealtimePayload,
} from './realtime';

export interface RedisStreamConfig {
  /** Redis URL. Used only when client is not supplied. */
  url?: string;
  /** Redis command client. Must support XADD, XREAD, and XTRIM via command(). */
  client?: RedisCommandSource;
  /** Prefix for Redis stream keys. */
  keyPrefix?: string;
  /** Approximate maximum entries retained per stream. */
  maxLen?: number;
  /** Approximate replay retention. Implemented with XTRIM MINID using Redis stream ids. */
  replayTtlMs?: number;
  /** XREAD BLOCK timeout. Default: 5000ms. */
  blockMs?: number;
  /** Maximum entries read per XREAD call. Default: 100. */
  count?: number;
  /** Message codec. Defaults to JSON. */
  codec?: RealtimeCodec;
  /** Close client created from url when broker.close() is called. */
  closeClient?: boolean;
}

/**
 * Redis Streams realtime broker.
 *
 * This mode is designed for SSE-style fanout with bounded replay. It does not
 * use consumer groups, so each subscriber can independently resume from an SSE
 * Last-Event-ID without stealing messages from other clients.
 */
export class RedisStreamRealtimeBroker implements RealtimeBroker {
  private readonly client: RedisCommandClient;
  private readonly keyPrefix: string | undefined;
  private readonly maxLen: number | undefined;
  private readonly replayTtlMs: number | undefined;
  private readonly blockMs: number;
  private readonly count: number;
  private readonly codec: RealtimeCodec;
  private readonly closeClient: boolean;

  constructor(config: RedisStreamConfig) {
    const source = config.client ?? (config.url ? createBunRedisClient(config.url) : undefined);
    if (!source) {
      throw new Error('Redis Stream broker requires a url or command client.');
    }

    this.client = toRedisCommandClient(source);
    this.keyPrefix = config.keyPrefix;
    this.maxLen = config.maxLen;
    this.replayTtlMs = config.replayTtlMs;
    this.blockMs = config.blockMs ?? 5000;
    this.count = config.count ?? 100;
    this.codec = config.codec ?? jsonRealtimeCodec;
    this.closeClient = config.closeClient ?? Boolean(config.url);
  }

  async publish<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    payload: InferSchema<S>,
    options?: RealtimePublishOptions,
  ): Promise<RealtimeMessage<InferSchema<S>>>;
  async publish<T = unknown>(topic: string, payload: T, options?: RealtimePublishOptions): Promise<RealtimeMessage<T>>;
  async publish<T = unknown>(
    topic: RealtimeTopic,
    payload: T,
    options?: RealtimePublishOptions,
  ): Promise<RealtimeMessage<T>> {
    validateRealtimePayload(topic, payload);
    const encoded = createRealtimeMessage(topic, payload, options);
    const streamKey = this.streamFor(encoded.topic);
    const args = this.xaddArgs(this.codec.encode(encoded));
    const redisId = await this.client.command('XADD', [streamKey, ...args]);
    await this.trimByTtl(streamKey);
    return decodeRealtimeMessage<T>(encoded, String(redisId));
  }

  async subscribe<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    handler: RealtimeHandler<InferSchema<S>>,
    options?: RealtimeSubscribeOptions,
  ): Promise<RealtimeSubscription>;
  async subscribe<T = unknown>(
    topic: string,
    handler: RealtimeHandler<T>,
    options?: RealtimeSubscribeOptions,
  ): Promise<RealtimeSubscription>;
  async subscribe<T = unknown>(
    topic: RealtimeTopic,
    handler: RealtimeHandler<T>,
    options?: RealtimeSubscribeOptions,
  ): Promise<RealtimeSubscription> {
    const streamKey = this.streamFor(getRealtimeTopicName(topic));
    const controller = new AbortController();
    const signal = options?.signal;
    const onAbort = () => controller.abort(signal?.reason);
    signal?.addEventListener('abort', onAbort, { once: true });

    let lastId = this.resolveInitialId(options?.from);
    const pump = (async () => {
      while (!controller.signal.aborted) {
        const response = await this.client.command('XREAD', [
          'BLOCK',
          String(this.blockMs),
          'COUNT',
          String(this.count),
          'STREAMS',
          streamKey,
          lastId,
        ]);

        const records = parseXReadResponse(response, streamKey);
        for (const record of records) {
          if (controller.signal.aborted) {
            break;
          }

          lastId = record.id;
          const encoded = this.codec.decode(record.message);
          const message = decodeRealtimeMessage<T>(encoded, record.id);
          const error = getRealtimePayloadError(topic, message.payload);
          if (error) {
            useLogger('events:realtime').error(`Dropping invalid realtime message ${message.id}: ${error}`);
            continue;
          }
          await handler(message);
        }
      }
    })();

    const close = async () => {
      controller.abort();
      signal?.removeEventListener('abort', onAbort);
      await pump.catch(() => undefined);
    };

    return { close };
  }

  async close(): Promise<void> {
    if (this.closeClient) {
      await this.client.close?.();
    }
  }

  private streamFor(topic: string): string {
    return resolveRealtimeKey(this.keyPrefix, topic);
  }

  private xaddArgs(message: string): string[] {
    const args: string[] = [];
    if (this.maxLen && this.maxLen > 0) {
      args.push('MAXLEN', '~', String(this.maxLen));
    }
    args.push('*', 'message', message);
    return args;
  }

  private async trimByTtl(streamKey: string): Promise<void> {
    if (!this.replayTtlMs || this.replayTtlMs <= 0) {
      return;
    }

    const cutoffId = `${Math.max(0, Date.now() - this.replayTtlMs)}-0`;
    await this.client.command('XTRIM', [streamKey, 'MINID', '~', cutoffId]);
  }

  private resolveInitialId(from: RealtimeSubscribeOptions['from']): string {
    if (!from || from === 'latest') {
      return '$';
    }
    if (from === 'earliest') {
      return '0-0';
    }
    return from;
  }
}

interface ParsedStreamRecord {
  id: string;
  message: string;
}

function parseXReadResponse(response: unknown, expectedStream: string): ParsedStreamRecord[] {
  if (!Array.isArray(response)) {
    return [];
  }

  const records: ParsedStreamRecord[] = [];
  for (const stream of response) {
    if (!Array.isArray(stream) || stream.length < 2) {
      continue;
    }

    const streamName = String(stream[0]);
    if (streamName !== expectedStream || !Array.isArray(stream[1])) {
      continue;
    }

    for (const entry of stream[1]) {
      if (!Array.isArray(entry) || entry.length < 2 || !Array.isArray(entry[1])) {
        continue;
      }

      const id = String(entry[0]);
      const fields = entry[1];
      const message = fieldValue(fields, 'message');
      if (message !== undefined) {
        records.push({ id, message });
      }
    }
  }
  return records;
}

function fieldValue(fields: unknown[], name: string): string | undefined {
  for (let i = 0; i < fields.length; i += 2) {
    if (String(fields[i]) === name) {
      return String(fields[i + 1]);
    }
  }
  return undefined;
}

export function redisStream(config: RedisStreamConfig): RedisStreamRealtimeBroker {
  return new RedisStreamRealtimeBroker(config);
}
