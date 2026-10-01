// ---------------------------------------------------------------------------
// Message — the envelope delivered to handlers
// ---------------------------------------------------------------------------

/**
 * A message received by a handler.
 *
 * @typeParam T - The typed payload (inferred from the TopicDefinition)
 */
export interface Message<T> {
  /** Unique message ID (for deduplication / logging) */
  readonly id: string;
  /** The topic this message was published to */
  readonly topic: string;
  /** Optional logical routing channel from the topic definition */
  readonly channel?: string;
  /** The typed, validated payload */
  readonly payload: T;
  /** Optional routing/partition key supplied by the publisher */
  readonly key?: string;
  /** Optional publisher-supplied deduplication key */
  readonly dedupeKey?: string;
  /** Optional topic contract version */
  readonly topicVersion?: string;
  /** When the message was published */
  readonly timestamp: Date;
  /** Optional key-value attributes for routing / filtering */
  readonly attributes: Readonly<Record<string, string>>;
  /** Current delivery attempt (1-based) */
  readonly attempt: number;
  /** Trace ID for distributed tracing / correlation */
  readonly traceId?: string;
  /** Aborted when handler timeout expires or the transport stops waiting */
  readonly signal: AbortSignal;
  /**
   * Acknowledge the message (manual ack mode only).
   * In auto-ack mode this is a no-op.
   */
  ack(): void;
  /**
   * Negatively acknowledge — triggers retry or DLQ.
   * In auto-ack mode, throw from the handler instead.
   */
  nack(reason?: string): void;
}

/** Options for publishing a message */
export interface PublishOptions {
  /** Stable message ID. Generated when omitted. */
  messageId?: string;
  /** Routing/partition key for transports that support keyed delivery */
  key?: string;
  /** Idempotency key used by handlers or transports for deduplication */
  dedupeKey?: string;
  /** Key-value attributes attached to the message envelope */
  attributes?: Record<string, string>;
  /** Trace ID for correlation. Auto-generated if omitted. */
  traceId?: string;
  /** W3C trace parent propagated by HTTP-based transports. */
  traceparent?: string;
  /** W3C trace state propagated by HTTP-based transports. */
  tracestate?: string;
  /** Cancel publication. A cancellation after send has an ambiguous outcome. */
  signal?: AbortSignal;
  /** Absolute deadline as epoch milliseconds or a Date. Earlier than the transport default wins. */
  deadline?: number | Date;
}
