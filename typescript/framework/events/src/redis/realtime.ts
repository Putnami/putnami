import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { validateSchema } from '@putnami/runtime';
import type { TopicDefinition } from '../topic/topic';

export type RealtimeTopic<S extends SchemaDefinition = SchemaDefinition> = TopicDefinition<S> | string;

export interface RealtimePublishOptions {
  /** Publisher-supplied event id. Stream mode uses the Redis stream id as the public id. */
  id?: string;
  /** Key-value attributes attached to the realtime message. */
  attributes?: Record<string, string>;
  /** Trace id for correlation with request/event logs. */
  traceId?: string;
}

export interface RealtimeSubscribeOptions {
  /**
   * Replay position for stream mode. Use an SSE Last-Event-ID value to resume
   * after that id. Pub/Sub mode ignores this because it is live-only.
   */
  from?: 'latest' | 'earliest' | string;
  /** Optional abort signal for the subscription loop. */
  signal?: AbortSignal;
}

export interface RealtimeMessage<T = unknown> {
  readonly id: string;
  readonly topic: string;
  readonly payload: T;
  readonly timestamp: Date;
  readonly attributes: Readonly<Record<string, string>>;
  readonly traceId?: string;
}

export interface RealtimeSubscription {
  close(): Promise<void>;
}

export type RealtimeHandler<T> = (message: RealtimeMessage<T>) => void | Promise<void>;

export interface RealtimeBroker {
  publish<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    payload: InferSchema<S>,
    options?: RealtimePublishOptions,
  ): Promise<RealtimeMessage<InferSchema<S>>>;
  publish<T = unknown>(topic: string, payload: T, options?: RealtimePublishOptions): Promise<RealtimeMessage<T>>;
  subscribe<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    handler: RealtimeHandler<InferSchema<S>>,
    options?: RealtimeSubscribeOptions,
  ): Promise<RealtimeSubscription>;
  subscribe<T = unknown>(
    topic: string,
    handler: RealtimeHandler<T>,
    options?: RealtimeSubscribeOptions,
  ): Promise<RealtimeSubscription>;
}

export interface EncodedRealtimeMessage {
  id: string;
  topic: string;
  payload: unknown;
  timestamp: string;
  attributes: Record<string, string>;
  traceId?: string;
}

export interface RealtimeCodec {
  encode(message: EncodedRealtimeMessage): string;
  decode(raw: string): EncodedRealtimeMessage;
}

export const jsonRealtimeCodec: RealtimeCodec = {
  encode(message) {
    return JSON.stringify(message);
  },
  decode(raw) {
    return JSON.parse(raw) as EncodedRealtimeMessage;
  },
};

export function getRealtimeTopicName(topic: RealtimeTopic): string {
  return typeof topic === 'string' ? topic : topic.name;
}

/**
 * Validate a realtime payload against the topic schema without throwing.
 *
 * Returns a human-readable error string when the payload is invalid, or `null`
 * when it is valid (or the topic is an untyped string, which carries no schema).
 * Shared by the publish (throwing) and consume (log-and-drop) paths so both
 * enforce the exact same contract.
 */
export function getRealtimePayloadError(topic: RealtimeTopic, payload: unknown): string | null {
  if (typeof topic === 'string') {
    return null;
  }

  const { errors } = validateSchema(topic.schema, payload, { label: topic.name });
  if (errors.length === 0) {
    return null;
  }

  const messages = errors.map((e) => `${e.field}: ${e.message}`).join('; ');
  return `Invalid payload for topic '${topic.name}': ${messages}`;
}

export function validateRealtimePayload(topic: RealtimeTopic, payload: unknown): void {
  const error = getRealtimePayloadError(topic, payload);
  if (error) {
    throw new Error(error);
  }
}

export function createRealtimeMessage<T>(
  topic: RealtimeTopic,
  payload: T,
  options?: RealtimePublishOptions,
): EncodedRealtimeMessage {
  return {
    id: options?.id ?? crypto.randomUUID(),
    topic: getRealtimeTopicName(topic),
    payload,
    timestamp: new Date().toISOString(),
    attributes: options?.attributes ?? {},
    traceId: options?.traceId,
  };
}

export function decodeRealtimeMessage<T>(encoded: EncodedRealtimeMessage, overrideId?: string): RealtimeMessage<T> {
  return {
    id: overrideId ?? encoded.id,
    topic: encoded.topic,
    payload: encoded.payload as T,
    timestamp: new Date(encoded.timestamp),
    attributes: encoded.attributes,
    traceId: encoded.traceId,
  };
}

export function resolveRealtimeKey(prefix: string | undefined, topic: string): string {
  return prefix ? `${prefix}:${topic}` : topic;
}
