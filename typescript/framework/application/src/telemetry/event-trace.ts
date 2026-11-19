/**
 * Event trace conventions — the telemetry protocol's rule, which the Go
 * runtime follows too (`protocols/telemetry/event_trace.go`).
 *
 * A publish inside a span writes the span's W3C trace context into the event
 * attributes, and the delivery of that event runs its handler in a consumer
 * span that continues the trace, or starts a new root without one. The values
 * are committed in `protocols/telemetry/fixtures/conventions/event-trace-context.json`,
 * which the tests of both runtimes read.
 */

import {
  type ActiveTelemetrySpan,
  activeTelemetryContext,
  startTelemetrySpan,
  type TelemetryTraceContext,
} from './client-telemetry';

/** The event attribute that carries the publishing span's W3C traceparent. */
export const ATTR_EVENT_TRACEPARENT = 'traceparent';
/** The event attribute that carries the publishing span's W3C tracestate. */
export const ATTR_EVENT_TRACESTATE = 'tracestate';
/** The longest tracestate a consumer continues; a longer one is dropped. */
export const MAX_EVENT_TRACESTATE_LENGTH = 512;
/** The consumer span name starts with it and ends with the topic. */
export const EVENT_CONSUMER_SPAN_NAME_PREFIX = 'process ';
/** The OTLP kind of the consumer span: consumer. */
export const EVENT_CONSUMER_SPAN_KIND = 5;
export const ATTR_MESSAGING_OPERATION = 'messaging.operation.name';
export const MESSAGING_OPERATION_PROCESS = 'process';
export const ATTR_MESSAGING_DESTINATION = 'messaging.destination.name';
export const ATTR_MESSAGING_MESSAGE_ID = 'messaging.message.id';
/** The status message of a consumer span whose handler failed. */
export const EVENT_CONSUMER_FAILURE_MESSAGE = 'events.handler_failed';

const TRACEPARENT = /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/;
const ZERO_TRACE_ID = '0'.repeat(32);
const ZERO_SPAN_ID = '0'.repeat(16);

/**
 * A copy of `attributes` with the context's traceparent and, when it has one,
 * its tracestate; a caller value of either is replaced, and a tracestate the
 * context lacks is removed.
 */
export function writeEventTraceAttributes(
  attributes: Readonly<Record<string, string>>,
  context: TelemetryTraceContext,
): Record<string, string> {
  const out: Record<string, string> = { ...attributes };
  out[ATTR_EVENT_TRACEPARENT] = `00-${context.traceId}-${context.spanId}-${context.traceFlags}`;
  if (context.tracestate) out[ATTR_EVENT_TRACESTATE] = context.tracestate;
  else delete out[ATTR_EVENT_TRACESTATE];
  return out;
}

/**
 * The trace context the attributes of an event carry, or undefined when the
 * traceparent is missing or invalid: version 00, a non-zero trace id, a
 * non-zero span id and 2-hex flags, all lowercase. A tracestate longer than
 * {@link MAX_EVENT_TRACESTATE_LENGTH} or holding a control character is dropped.
 */
export function readEventTraceAttributes(
  attributes: Readonly<Record<string, string>> | undefined,
): TelemetryTraceContext | undefined {
  const match = TRACEPARENT.exec(attributes?.[ATTR_EVENT_TRACEPARENT] ?? '');
  if (!match || match[1] === ZERO_TRACE_ID || match[2] === ZERO_SPAN_ID) return undefined;
  const tracestate = attributes?.[ATTR_EVENT_TRACESTATE];
  const keepState = !!tracestate && tracestate.length <= MAX_EVENT_TRACESTATE_LENGTH && !/[\0\r\n]/.test(tracestate);
  return {
    traceId: match[1] as string,
    spanId: match[2] as string,
    traceFlags: match[3] as string,
    ...(keepState ? { tracestate } : {}),
  };
}

/**
 * The attributes a publish in the current context sends: with the active
 * span's trace context, or unchanged outside a span.
 */
export function eventPublishAttributes(attributes: Record<string, string>): Record<string, string> {
  const context = activeTelemetryContext();
  return context ? writeEventTraceAttributes(attributes, context) : attributes;
}

/** The name of the consumer span of a topic. */
export function eventConsumerSpanName(topic: string): string {
  return EVENT_CONSUMER_SPAN_NAME_PREFIX + topic;
}

/**
 * Start the consumer span of one delivery: the child of the span the event's
 * traceparent names, or a new root whatever span delivers the event.
 */
export function startEventConsumerSpan(event: {
  readonly topic: string;
  readonly id: string;
  readonly attributes?: Readonly<Record<string, string>>;
}): ActiveTelemetrySpan {
  const parent = readEventTraceAttributes(event.attributes);
  return startTelemetrySpan({
    name: eventConsumerSpanName(event.topic),
    kind: 'consumer',
    newRoot: true,
    ...(parent ? { remoteContext: parent } : {}),
    attributes: {
      [ATTR_MESSAGING_OPERATION]: MESSAGING_OPERATION_PROCESS,
      [ATTR_MESSAGING_DESTINATION]: event.topic,
      [ATTR_MESSAGING_MESSAGE_ID]: event.id,
    },
  });
}
