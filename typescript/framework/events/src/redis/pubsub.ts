import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import type { TopicDefinition } from '../topic/topic';
import { createBunRedisClient, type RedisPubSubClient } from './bun-redis';
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

export interface RedisPubSubConfig {
  /** Redis URL. Used only when publisher/subscriber are not supplied. */
  url?: string;
  /** Prefix for Redis channels. */
  keyPrefix?: string;
  /** Publisher client. */
  publisher?: RedisPubSubClient;
  /** Subscriber client. Defaults to publisher.duplicate() when available, otherwise publisher. */
  subscriber?: RedisPubSubClient;
  /** Message codec. Defaults to JSON. */
  codec?: RealtimeCodec;
  /** Close clients created from url when broker.close() is called. */
  closeClients?: boolean;
}

/**
 * Redis Pub/Sub realtime broker.
 *
 * This mode is live-only fanout. It intentionally does not support replay,
 * ack, retry, DLQ, or competing consumers.
 */
export class RedisPubSubRealtimeBroker implements RealtimeBroker {
  private readonly publisher: RedisPubSubClient;
  private readonly subscriber: RedisPubSubClient;
  private readonly keyPrefix: string | undefined;
  private readonly codec: RealtimeCodec;
  private readonly closeClients: boolean;

  constructor(config: RedisPubSubConfig) {
    const publisher = config.publisher ?? (config.url ? createBunRedisClient(config.url) : undefined);
    if (!publisher) {
      throw new Error('Redis Pub/Sub broker requires a url or publisher client.');
    }

    this.publisher = publisher;
    this.subscriber = config.subscriber ?? publisher.duplicate?.() ?? publisher;
    this.keyPrefix = config.keyPrefix;
    this.codec = config.codec ?? jsonRealtimeCodec;
    this.closeClients = config.closeClients ?? Boolean(config.url);
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
    await this.publisher.publish(this.channelFor(encoded.topic), this.codec.encode(encoded));
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
    const topicName = getRealtimeTopicName(topic);
    const channel = this.channelFor(topicName);
    const listener = (raw: string) => {
      if (options?.signal?.aborted) {
        return;
      }
      const encoded = this.codec.decode(raw);
      const message = decodeRealtimeMessage<T>(encoded);
      const error = getRealtimePayloadError(topic, message.payload);
      if (error) {
        useLogger('events:realtime').error(`Dropping invalid realtime message ${message.id}: ${error}`);
        return;
      }
      void handler(message);
    };

    await this.subscriber.subscribe(channel, listener);

    const close = async () => {
      await this.subscriber.unsubscribe?.(channel, listener);
    };
    options?.signal?.addEventListener('abort', () => {
      void close();
    });

    return { close };
  }

  async close(): Promise<void> {
    if (!this.closeClients) {
      return;
    }

    await this.subscriber.close?.();
    if (this.subscriber !== this.publisher) {
      await this.publisher.close?.();
    }
  }

  private channelFor(topic: string): string {
    return resolveRealtimeKey(this.keyPrefix, topic);
  }
}

export function redisPubSub(config: RedisPubSubConfig): RedisPubSubRealtimeBroker {
  return new RedisPubSubRealtimeBroker(config);
}
