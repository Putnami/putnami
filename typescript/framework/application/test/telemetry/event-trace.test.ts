import { afterEach, describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import type { TelemetryTraceContext } from '../../src/telemetry/client-telemetry';
import {
  ATTR_EVENT_TRACEPARENT,
  ATTR_EVENT_TRACESTATE,
  ATTR_MESSAGING_DESTINATION,
  ATTR_MESSAGING_MESSAGE_ID,
  ATTR_MESSAGING_OPERATION,
  EVENT_CONSUMER_FAILURE_MESSAGE,
  EVENT_CONSUMER_SPAN_KIND,
  EVENT_CONSUMER_SPAN_NAME_PREFIX,
  eventConsumerSpanName,
  eventPublishAttributes,
  MAX_EVENT_TRACESTATE_LENGTH,
  MESSAGING_OPERATION_PROCESS,
  readEventTraceAttributes,
  startEventConsumerSpan,
} from '../../src/telemetry/event-trace';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';

interface FixtureContext {
  traceId: string;
  spanId: string;
  flags: string;
  tracestate?: string;
}

interface EventTraceFixture {
  attributes: { traceparent: string; tracestate: string };
  maxTracestateLength: number;
  consumerSpan: {
    namePrefix: string;
    kind: number;
    attributes: { operation: string; destination: string; messageId: string };
    operation: string;
    failureMessage: string;
  };
  publish: Array<{
    name: string;
    span?: FixtureContext;
    callerAttributes: Record<string, string>;
    attributes: Record<string, string>;
  }>;
  consume: Array<{
    name: string;
    attributes: Record<string, string>;
    continues: boolean;
    parent?: FixtureContext;
  }>;
}

const fixture = JSON.parse(
  readFileSync(
    join(import.meta.dir, '../../../../../protocols/telemetry/fixtures/conventions/event-trace-context.json'),
    'utf8',
  ),
) as EventTraceFixture;

const AMBIENT: TelemetryTraceContext = {
  traceId: '99999999999999999999999999999999',
  spanId: '8888888888888888',
  traceFlags: '01',
};

function traceContext(context: FixtureContext): TelemetryTraceContext {
  return {
    traceId: context.traceId,
    spanId: context.spanId,
    traceFlags: context.flags,
    ...(context.tracestate ? { tracestate: context.tracestate } : {}),
  };
}

describe('event trace conventions', () => {
  let collector: TelemetryCollector | undefined;

  afterEach(() => {
    if (collector) clearCollector(collector);
    collector = undefined;
  });

  it('keeps the attribute names and the consumer span equal to the shared fixture', () => {
    expect(fixture.attributes).toEqual({ traceparent: ATTR_EVENT_TRACEPARENT, tracestate: ATTR_EVENT_TRACESTATE });
    expect(fixture.maxTracestateLength).toBe(MAX_EVENT_TRACESTATE_LENGTH);
    expect(fixture.consumerSpan).toEqual({
      namePrefix: EVENT_CONSUMER_SPAN_NAME_PREFIX,
      kind: EVENT_CONSUMER_SPAN_KIND,
      attributes: {
        operation: ATTR_MESSAGING_OPERATION,
        destination: ATTR_MESSAGING_DESTINATION,
        messageId: ATTR_MESSAGING_MESSAGE_ID,
      },
      operation: MESSAGING_OPERATION_PROCESS,
      failureMessage: EVENT_CONSUMER_FAILURE_MESSAGE,
    });
    expect(eventConsumerSpanName('orders.created')).toBe('process orders.created');
  });

  specTest(
    'publishes and consumes every case of the shared fixture',
    {
      feature: 'telemetry/otlp-push-contract',
      requirement: 'event-trace-context-is-shared',
      check: 'the-typescript-runtime-follows-the-event-trace-fixture',
    },
    async () => {
      expect(fixture.publish.length).toBeGreaterThan(0);
      expect(fixture.consume.length).toBeGreaterThan(0);

      for (const c of fixture.publish) {
        const caller = { ...c.callerAttributes };
        const publish = () => eventPublishAttributes(caller);
        const got = c.span ? await runInContext({ __putnamiTrace: traceContext(c.span) }, publish) : publish();
        expect({ name: c.name, attributes: got }).toEqual({ name: c.name, attributes: c.attributes });
        expect(caller).toEqual(c.callerAttributes);
      }

      collector = new TelemetryCollector();
      setCollector(collector);
      for (const [i, c] of fixture.consume.entries()) {
        const id = `fixture-${i}`;
        // An ambient span must never become the consumer's parent.
        const span = await runInContext({ __putnamiTrace: AMBIENT }, () =>
          startEventConsumerSpan({ topic: id, id, attributes: c.attributes }),
        );
        span.finish();
        const recorded = collector.drainSpans();

        expect({ name: c.name, continues: readEventTraceAttributes(c.attributes) !== undefined }).toEqual({
          name: c.name,
          continues: c.continues,
        });
        if (!c.continues || !c.parent) {
          expect(span.context.traceId).not.toBe(AMBIENT.traceId);
          expect(recorded).toHaveLength(1);
          expect(recorded[0]?.parentSpanId).toBeUndefined();
          continue;
        }
        expect(span.context.traceId).toBe(c.parent.traceId);
        expect(span.context.traceFlags).toBe(c.parent.flags);
        expect(span.context.tracestate).toBe(c.parent.tracestate);
        const sampled = (Number.parseInt(c.parent.flags, 16) & 1) === 1;
        expect(recorded).toHaveLength(sampled ? 1 : 0);
        if (sampled) expect(recorded[0]?.parentSpanId).toBe(c.parent.spanId);
      }
    },
  );

  it('records the consumer span with its kind, its attributes and a failure message', () => {
    collector = new TelemetryCollector();
    setCollector(collector);
    const span = startEventConsumerSpan({ topic: 'orders.created', id: 'evt-1', attributes: {} });
    span.finish({ code: EVENT_CONSUMER_FAILURE_MESSAGE });
    const [recorded] = collector.drainSpans();
    expect(recorded?.name).toBe('process orders.created');
    expect(recorded?.kind).toBe(EVENT_CONSUMER_SPAN_KIND);
    expect(recorded?.statusCode).toBe(2);
    expect(recorded?.statusMessage).toBe(EVENT_CONSUMER_FAILURE_MESSAGE);
    expect(recorded?.attributes).toMatchObject({
      [ATTR_MESSAGING_OPERATION]: MESSAGING_OPERATION_PROCESS,
      [ATTR_MESSAGING_DESTINATION]: 'orders.created',
      [ATTR_MESSAGING_MESSAGE_ID]: 'evt-1',
    });
  });

  it('drops a tracestate holding a control character and still continues the trace', () => {
    const parent = readEventTraceAttributes({
      traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
      tracestate: 'vendor=a\nb',
    });
    expect(parent).toEqual({
      traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
      spanId: '00f067aa0ba902b7',
      traceFlags: '01',
    });
  });
});
