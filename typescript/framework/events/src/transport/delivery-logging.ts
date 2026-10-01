import { useLogger } from '@putnami/runtime';
import type { Envelope } from './transport';

// ---------------------------------------------------------------------------
// Event boundary log contract — the TypeScript twin of Go's
// `go/framework/events/delivery_logging.go`.
//
// Every record shape of the event boundary is built here exactly once, so a
// transport can neither invent a message, a severity, nor a field name. The
// contract itself is normative in `protocols/logging/conformance` (manifest +
// README); both runtimes must emit schema-equivalent records.
// ---------------------------------------------------------------------------

/**
 * Pinned logger names of the contract ("Pinned logger names": dots, never
 * colons). `events.handler` owns the single terminal record per delivery,
 * `events.broker` owns the retry / dead-letter / drop records, and
 * `events.publish` owns the publish record.
 */
export const EVENT_HANDLER_LOGGER = 'events.handler';
export const EVENT_BROKER_LOGGER = 'events.broker';
export const EVENT_PUBLISH_LOGGER = 'events.publish';

/** Outcome vocabulary. Event deliveries only ever report success or failure. */
export const OUTCOME_SUCCESS = 'success';
export const OUTCOME_FAILURE = 'failure';

/**
 * Drop reasons reported as `event.reason` on a `message dropped` record. Closed
 * vocabulary — kept identical to the Go constants of the same name so a
 * dashboard filtering on a reason matches both runtimes.
 */
export const DROP_QUEUE_FULL = 'queue_full';
export const DROP_RETRY_ENQUEUE_FAILED = 'retry_enqueue_failed';
export const DROP_DLQ_ENQUEUE_FAILED = 'dlq_enqueue_failed';
export const DROP_DLQ_NO_SUBSCRIBER = 'dlq_no_subscriber';
export const DROP_RETRIES_EXHAUSTED = 'retries_exhausted';

/** One of the closed drop reasons above. */
export type DropReason =
  | typeof DROP_QUEUE_FULL
  | typeof DROP_RETRY_ENQUEUE_FAILED
  | typeof DROP_DLQ_ENQUEUE_FAILED
  | typeof DROP_DLQ_NO_SUBSCRIBER
  | typeof DROP_RETRIES_EXHAUSTED;

/**
 * The base `event` group shared by every record of one delivery: the topic, the
 * message id, and the 1-based attempt of THIS delivery (never `attempt + 1` —
 * only `nextAttempt` adds one).
 */
export function eventGroup(envelope: Envelope, extra?: Record<string, unknown>): Record<string, unknown> {
  return {
    topic: envelope.topic,
    messageId: envelope.id,
    attempt: envelope.attempt,
    ...extra,
  };
}

/**
 * Renders a delivery's event group as the single structured data param of a log
 * call. The JSON sink flattens a lone object param over the entry's context, so
 * a broker record carries exactly the fields it declares — never leftovers of
 * the terminal record's group (such as `durationMs`) that the delivery's log
 * context still holds. This mirrors Go's closed `slog.Attr` semantics.
 */
export function eventFields(envelope: Envelope, extra?: Record<string, unknown>): { event: Record<string, unknown> } {
  return { event: eventGroup(envelope, extra) };
}

/**
 * Coerce a thrown value into a real `Error` so it reaches the logger's
 * structured `error` field instead of being stringified into the message or
 * parked in `data`.
 */
export function asError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value));
}

/** The failure text carried in `dlq.error` attributes (never in a log message). */
export function errorText(value: unknown): string {
  return value instanceof Error ? value.message : String(value);
}

/**
 * Emit the contract's retry record: WARNING (the failure is recoverable, so the
 * structured error travels as a param rather than promoting the record to
 * ERROR), the failed delivery's `attempt`, the scheduled `nextAttempt`, the
 * backoff `delayMs`, and `maxRetries` in the same unit as `attempt` (maximum
 * total delivery attempts).
 */
export function logRetryScheduled(envelope: Envelope, delayMs: number, maxRetries: number, error: unknown): void {
  useLogger(EVENT_BROKER_LOGGER).warn(
    'message retry scheduled',
    asError(error),
    eventFields(envelope, {
      nextAttempt: envelope.attempt + 1,
      delayMs,
      maxRetries,
      outcome: OUTCOME_FAILURE,
    }),
  );
}

/**
 * Emit the contract's drop record: ERROR, because the message is lost. `reason`
 * is one of the closed vocabulary values; `error` is optional (a queue-full drop
 * has no handler error — the delivery never ran) and is omitted from the params
 * entirely when absent so the sink still flattens the event group.
 */
export function logDropped(
  envelope: Envelope,
  reason: DropReason,
  error?: unknown,
  extra?: Record<string, unknown>,
): void {
  const logger = useLogger(EVENT_BROKER_LOGGER);
  const fields = eventFields(envelope, { reason, ...extra, outcome: OUTCOME_FAILURE });
  if (error === undefined) {
    logger.error('message dropped', fields);
    return;
  }
  logger.error('message dropped', asError(error), fields);
}

/**
 * Emit the contract's dead-letter record: ERROR, once per dead-letter (never per
 * `.dlq` subscriber), and only once the dead-letter has actually been accepted —
 * an operator must never be pointed at a DLQ the message never reached. The
 * group describes the ORIGINAL delivery (its topic and true final attempt) plus
 * the `dlqTopic` it was routed to.
 */
export function logDeadLettered(envelope: Envelope, dlqTopic: string, error: unknown): void {
  useLogger(EVENT_BROKER_LOGGER).error(
    'message dead-lettered',
    asError(error),
    eventFields(envelope, { dlqTopic, outcome: OUTCOME_FAILURE }),
  );
}
