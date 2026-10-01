import type { InferSchema, SchemaDefinition } from '@putnami/runtime';
import { useLogger, validateSchema } from '@putnami/runtime';
import { incCounter } from '@putnami/application';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import type { PublishOptions } from '../topic/message';
import type { TopicDefinition } from '../topic/topic';
import { EVENT_PUBLISH_LOGGER } from '../transport/delivery-logging';
import type { Transport } from '../transport/transport';
import { buildEnvelope } from '../transport/transport';

// ---------------------------------------------------------------------------
// Publisher — typed publish function bound to a topic
// ---------------------------------------------------------------------------

/**
 * A typed publish function for a specific topic.
 * Validates the payload against the topic schema before sending.
 */
export type PublishFn<S extends SchemaDefinition> = (
  payload: InferSchema<S>,
  options?: PublishOptions,
) => Promise<void>;

// ---------------------------------------------------------------------------
// Transport registry — set by the events plugin during warmup
// ---------------------------------------------------------------------------

let activeTransport: Transport | undefined;

/** @internal Set the active transport. Called by the events plugin. */
export function setTransport(transport: Transport): void {
  activeTransport = transport;
}

/** @internal Get the active transport. */
export function getTransport(): Transport | undefined {
  return activeTransport;
}

/** @internal Clear the transport (for testing). */
export function clearTransport(): void {
  activeTransport = undefined;
}

// ---------------------------------------------------------------------------
// Published-topic registry — feeds the infra requirements scratch fragment
// ---------------------------------------------------------------------------

const publishedTopics = new Set<string>();

export interface DesignPublication {
  topic: TopicDefinition;
  source?: { path: string; line?: number; symbol?: string };
}

const designPublications = new Map<string, DesignPublication>();
const designPublicationTopics = new Set<string>();

/**
 * @internal Record that a topic has a publisher. Called whenever
 * {@link getPublisher} is invoked (typically at module top level), so that
 * the events plugin's `generate()` hook can derive the set of published
 * topics by importing the discovered handler modules.
 */
function recordPublishedTopic(name: string): void {
  publishedTopics.add(name);
}

/** @internal Snapshot the recorded published topic names. */
export function getPublishedTopics(): string[] {
  return [...publishedTopics];
}

/** @internal Native publisher call sites retained across infra re-scan resets. */
export function getDesignPublications(): DesignPublication[] {
  return [...designPublications.values()];
}

/**
 * @internal Clear recorded publisher call sites. `designPublications` is a
 * process-global that deliberately survives {@link clearPublishedTopics}.
 * Event discovery resets both registries at each project boundary; isolated
 * callers and tests can use this function to establish the same boundary.
 */
export function clearDesignPublications(): void {
  designPublications.clear();
  designPublicationTopics.clear();
}

/** @internal Clear the recorded published topics (for testing and re-runs). */
export function clearPublishedTopics(): void {
  publishedTopics.clear();
}

// ---------------------------------------------------------------------------
// getPublisher() — create a typed publish function
// ---------------------------------------------------------------------------

/**
 * Create a typed publish function for a topic.
 *
 * The returned function validates the payload against the topic's schema
 * before publishing. It uses the transport configured by the events plugin.
 *
 * Emits telemetry:
 * - `events.publish.{topic}` counter on successful publish
 * - `events.publish.{topic}.error` counter on validation/transport failure
 *
 * @param topic - The TopicDefinition to publish to
 * @returns A typed, schema-validated publish function
 * @throws If no transport is configured (events plugin not started)
 *
 * @example
 * ```typescript
 * import { getPublisher } from '@putnami/events';
 * import { UserCreated } from '../shared/topics';
 *
 * const publish = getPublisher(UserCreated);
 *
 * await publish({ id: '...', email: 'user@example.com', name: 'Jane' });
 * // Payload is type-checked and schema-validated before sending
 * ```
 */
export function getPublisher<S extends SchemaDefinition>(topicDef: TopicDefinition<S>): PublishFn<S> {
  recordPublishedTopic(topicDef.name);
  // getExternalCaller() throws and parses a full stack trace. This is a public
  // runtime API — the "call it at module top level" contract is documented, not
  // enforced — so capture the call site once per topic. A second publisher for a
  // topic already recorded keeps the first site rather than paying for a stack
  // walk on a path that may run per request.
  if (!designPublicationTopics.has(topicDef.name)) {
    designPublicationTopics.add(topicDef.name);
    const caller = getExternalCaller(getProjectRoot());
    const source = caller
      ? {
          path: caller.filePath,
          line: caller.lineNumber,
          ...(caller.functionName ? { symbol: caller.functionName } : {}),
        }
      : undefined;
    designPublications.set(`${topicDef.name}\0${source?.path ?? ''}`, {
      topic: topicDef,
      ...(source ? { source } : {}),
    });
  }

  return async (payload: InferSchema<S>, options?: PublishOptions): Promise<void> => {
    const transport = activeTransport;
    if (!transport) {
      throw new Error(
        `No transport configured. Ensure the events plugin is started before publishing to '${topicDef.name}'.`,
      );
    }

    // Validate payload against topic schema
    const { errors } = validateSchema(topicDef.schema, payload, { label: topicDef.name });
    if (errors.length > 0) {
      const messages = errors.map((e) => `${e.field}: ${e.message}`).join('; ');
      incCounter(`events.publish.${topicDef.name}.error`);
      throw new Error(`Invalid payload for topic '${topicDef.name}': ${messages}`);
    }

    const envelope = buildEnvelope(topicDef, payload, options);
    await transport.publish(topicDef.name, envelope, options);

    // Accumulate onto the CALLER's boundary, and only once the transport has
    // accepted the message (Go parity: a failed publish never claims one).
    // Inside a request `.append()`/`.increment()` mutate that request's log
    // context and return the same logger, so N publishes summarize on the
    // request's terminal record as `event.publishes` + `event.publishCount` (the
    // paired counter survives the append cap). Detached, the chain returns a NEW
    // logger carrying the fields — which is why the return value must be used.
    // The record's own `event` group stays {topic, messageId}, like Go's attr.
    useLogger(EVENT_PUBLISH_LOGGER)
      .append('event.publishes', { topic: topicDef.name, messageId: envelope.id })
      .increment('event.publishCount')
      .debug('message published', { event: { topic: topicDef.name, messageId: envelope.id } });

    // Telemetry: publish counter
    incCounter(`events.publish.${topicDef.name}`);
  };
}
