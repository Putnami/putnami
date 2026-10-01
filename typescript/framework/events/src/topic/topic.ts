import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';

// ---------------------------------------------------------------------------
// TopicDefinition — the typed contract for a topic
// ---------------------------------------------------------------------------

const TOPIC_MARKER = 'putnami:topic' as const;

/**
 * A typed topic definition. Topics are pure data — they carry no transport
 * or connection logic. Import them from shared code and use them with
 * `handler()` and `publish()`.
 *
 * @example
 * ```typescript
 * const UserCreated = topic('user.created', { id: Uuid, email: Email, name: String });
 * ```
 */
export interface TopicDefinition<S extends SchemaDefinition = SchemaDefinition> {
  readonly __topic: typeof TOPIC_MARKER;
  readonly name: string;
  readonly schema: S;
  /** Optional contract version for cross-service compatibility checks. */
  readonly version?: string;
  /** Optional logical channel used by routing transports. Not a physical transport name. */
  readonly channel?: string;
  /** Optional topic metadata. Values must be serializable strings. */
  readonly metadata?: Readonly<Record<string, string>>;
  /** @internal Native declaration location for build-time design discovery. */
  readonly __source?: { path: string; line?: number; symbol?: string };
}

export function isTopicDefinition(value: unknown): value is TopicDefinition {
  return typeof value === 'object' && value !== null && (value as TopicDefinition).__topic === TOPIC_MARKER;
}

/** Infer the payload type from a TopicDefinition */
export type InferTopicPayload<T> = T extends TopicDefinition<infer S> ? InferSchema<S> : never;

// ---------------------------------------------------------------------------
// topic() — create a typed topic definition
// ---------------------------------------------------------------------------

/**
 * Define a typed event topic.
 *
 * The topic is a pure contract: it holds the topic name and the payload schema.
 * It carries no transport logic and can be shared across services.
 *
 * @param name - The topic name (e.g. 'user.created', 'order.placed')
 * @param schema - A SchemaDefinition describing the payload shape
 * @returns A typed TopicDefinition
 *
 * @example
 * ```typescript
 * import { topic, schema } from '@putnami/events';
 * import { Uuid, Email } from '@putnami/runtime';
 *
 * const UserCreated = topic('user.created', { id: Uuid, email: Email, name: String });
 * // TopicDefinition<{ id: SchemaDescriptor<string>, email: SchemaDescriptor<string>, name: StringConstructor }>
 * ```
 */
export interface TopicOptions {
  /** Contract version attached to published envelopes. */
  version?: string;
  /** Logical channel used by event transport routing. */
  channel?: string;
  /** Static metadata attached to the topic definition. */
  metadata?: Record<string, string>;
}

export function topic<S extends SchemaDefinition>(name: string, schema: S, options?: TopicOptions): TopicDefinition<S> {
  const caller = getExternalCaller(getProjectRoot());
  return {
    __topic: TOPIC_MARKER,
    name,
    schema,
    version: options?.version,
    channel: options?.channel,
    metadata: options?.metadata,
    ...(caller
      ? {
          __source: {
            path: caller.filePath,
            line: caller.lineNumber,
            ...(caller.functionName ? { symbol: caller.functionName } : {}),
          },
        }
      : {}),
  };
}
