import { tryContext } from '@putnami/runtime';
import type { HandlerDefinition } from '../handler/handler';
import type { Message, PublishOptions } from '../topic/message';
import type { TopicDefinition } from '../topic/topic';
import { PUTNAMI_EVENTS_PROTOCOL, type EventEnvelope } from '../protocol';

// ---------------------------------------------------------------------------
// Transport — abstraction over the messaging backend
// ---------------------------------------------------------------------------

/** Internal message envelope used by transports */
export interface Envelope extends EventEnvelope {
  protocol: typeof PUTNAMI_EVENTS_PROTOCOL;
  id: string;
  topic: string;
  payload: unknown;
  channel?: string;
  key?: string;
  dedupeKey?: string;
  topicVersion?: string;
  timestamp: string;
  attributes: Record<string, string>;
  attempt: number;
  traceId?: string;
}

/**
 * Transport interface — the pluggable backend for event delivery.
 *
 * Implementations: in-memory (local dev), cloud proxy (production).
 * Users never interact with transports directly.
 */
export interface Transport {
  /**
   * Publish a fully-built {@link Envelope}.
   *
   * The envelope is self-describing: `envelope.topic` already carries the
   * destination topic. The leading `topic` argument is the same value, passed
   * separately so routing transports (and topic-name based backends such as
   * Google Pub/Sub) can dispatch without inspecting the envelope. For the
   * message payload itself, implementations must treat `envelope.topic` as
   * authoritative.
   *
   * @param topic - The topic to publish to (mirror of `envelope.topic`)
   * @param envelope - The fully-built event envelope to deliver
   * @param options - Publish controls and transport-specific propagation hints
   */
  publish(topic: string, envelope: Envelope, options?: PublishOptions): Promise<void>;

  /**
   * Subscribe a handler to a topic.
   * The transport is responsible for message delivery, retries, and DLQ.
   *
   * @param definition - The resolved handler definition
   * @param callback - The function to invoke with each message
   */
  subscribe(definition: HandlerDefinition, callback: (message: Message<unknown>) => Promise<void>): Promise<void>;

  /**
   * Start the transport (connect, begin consuming).
   */
  start(): Promise<void>;

  /**
   * Graceful shutdown: stop accepting new messages,
   * wait for in-flight handlers, nack unprocessed.
   */
  stop(): Promise<void>;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Generate a unique message ID */
export function generateMessageId(): string {
  return crypto.randomUUID();
}

/** Build an Envelope from a TopicDefinition and payload */
export function buildEnvelope(topic: TopicDefinition, payload: unknown, options?: PublishOptions): Envelope {
  const ctx = tryContext<{ traceId?: string; user?: Record<string, unknown> }>();

  // Merge caller-supplied attributes with auto-captured auth claims.
  // Auth attributes (auth.*) are protected — callers cannot override them.
  const autoAttrs = resolveAuthAttributes(ctx?.user) ?? {};
  const callerAttrs = Object.fromEntries(
    Object.entries(options?.attributes ?? {}).filter(([k]) => !k.startsWith('auth.')),
  );
  const attributes: Record<string, string> = { ...callerAttrs, ...autoAttrs };

  return {
    id: options?.messageId ?? generateMessageId(),
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    topic: topic.name,
    payload,
    channel: topic.channel,
    key: options?.key,
    dedupeKey: options?.dedupeKey,
    topicVersion: topic.version,
    timestamp: new Date().toISOString(),
    attributes,
    attempt: 1,
    traceId: options?.traceId ?? ctx?.traceId ?? crypto.randomUUID(),
  };
}

/** Extract serializable auth attributes from JWT user claims. */
function resolveAuthAttributes(user: Record<string, unknown> | undefined): Record<string, string> | undefined {
  if (!user) return undefined;
  const attrs: Record<string, string> = {};
  if (typeof user['sub'] === 'string') attrs['auth.sub'] = user['sub'];
  if (typeof user['email'] === 'string') attrs['auth.email'] = user['email'];
  if (typeof user['azp'] === 'string') attrs['auth.azp'] = user['azp'];
  if (typeof user['client_id'] === 'string') attrs['auth.client_id'] = user['client_id'];
  return Object.keys(attrs).length > 0 ? attrs : undefined;
}
