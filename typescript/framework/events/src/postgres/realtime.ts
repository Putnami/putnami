import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import type { TopicDefinition } from '../topic/topic';
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
} from '../redis/realtime';

export interface PostgresNotification {
  channel?: string;
  payload?: string;
}

export interface PostgresNotifyClient {
  query(sql: string, params?: unknown[]): Promise<unknown> | unknown;
  on?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
  off?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
  removeListener?(event: 'notification', handler: (notification: PostgresNotification) => void): unknown;
}

export interface PostgresRealtimeConfig {
  client: PostgresNotifyClient;
  /** Prefix for PostgreSQL notification channels. */
  channelPrefix?: string;
  /** Payload size guard. PostgreSQL NOTIFY payloads are limited to about 8000 bytes. Default: 7900. */
  maxPayloadBytes?: number;
  /** Message codec. Defaults to JSON. */
  codec?: RealtimeCodec;
}

/**
 * PostgreSQL LISTEN/NOTIFY realtime broker.
 *
 * This is live-only fanout for small apps that already run Postgres. It has no
 * replay and is not a reliable event-handler transport.
 */
export class PostgresRealtimeBroker implements RealtimeBroker {
  private readonly client: PostgresNotifyClient;
  private readonly channelPrefix: string | undefined;
  private readonly maxPayloadBytes: number;
  private readonly codec: RealtimeCodec;

  constructor(config: PostgresRealtimeConfig) {
    this.client = config.client;
    this.channelPrefix = config.channelPrefix;
    this.maxPayloadBytes = config.maxPayloadBytes ?? 7900;
    this.codec = config.codec ?? jsonRealtimeCodec;
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
    const raw = this.codec.encode(encoded);
    if (new TextEncoder().encode(raw).byteLength > this.maxPayloadBytes) {
      throw new Error(`Postgres NOTIFY payload for '${encoded.topic}' exceeds ${this.maxPayloadBytes} bytes.`);
    }

    await this.client.query('select pg_notify($1, $2)', [this.channelFor(encoded.topic), raw]);
    return decodeRealtimeMessage<T>(encoded);
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
    if (options?.from && options.from !== 'latest') {
      throw new Error('Postgres realtime is live-only and cannot replay from Last-Event-ID.');
    }

    const topicName = getRealtimeTopicName(topic);
    const channel = this.channelFor(topicName);
    const listener = (notification: PostgresNotification) => {
      if (options?.signal?.aborted || notification.channel !== channel || !notification.payload) {
        return;
      }

      const encoded = this.codec.decode(notification.payload);
      const message = decodeRealtimeMessage<T>(encoded);
      const error = getRealtimePayloadError(topic, message.payload);
      if (error) {
        useLogger('events:realtime').error(`Dropping invalid realtime message ${message.id}: ${error}`);
        return;
      }
      void handler(message);
    };

    this.client.on?.('notification', listener);
    await this.client.query(`LISTEN ${quoteIdentifier(channel)}`);

    const close = async () => {
      this.client.off?.('notification', listener);
      this.client.removeListener?.('notification', listener);
      await this.client.query(`UNLISTEN ${quoteIdentifier(channel)}`);
    };
    options?.signal?.addEventListener('abort', () => {
      void close();
    });

    return { close };
  }

  private channelFor(topic: string): string {
    return sanitizeChannel(resolveRealtimeKey(this.channelPrefix, topic));
  }
}

function sanitizeChannel(channel: string): string {
  return channel.replaceAll(/[^a-zA-Z0-9_]/g, '_').slice(0, 63);
}

function quoteIdentifier(identifier: string): string {
  return `"${identifier.replaceAll('"', '""')}"`;
}

export function postgresRealtime(config: PostgresRealtimeConfig): PostgresRealtimeBroker {
  return new PostgresRealtimeBroker(config);
}
