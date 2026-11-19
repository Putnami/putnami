import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  ATTR_EVENT_TRACEPARENT,
  ATTR_EVENT_TRACESTATE,
  EVENT_CONSUMER_FAILURE_MESSAGE,
  setCollector,
  startTelemetrySpan,
  TelemetryCollector,
  type TelemetryTraceContext,
} from '@putnami/application';
import { tryContext, Uuid } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { handler } from '../../src/handler/handler';
import { clearTransport, getPublisher, setTransport } from '../../src/publisher/publisher';
import { PUTNAMI_EVENTS_PROTOCOL } from '../../src/protocol';
import { MemoryBroker } from '../../src/server/memory-broker';
import { decodePushEnvelope } from '../../src/server/push-receiver';
import type { Message } from '../../src/topic/message';
import { topic } from '../../src/topic/topic';
import { eventServerTransport } from '../../src/transport/event-server.transport';
import type { Envelope, Transport } from '../../src/transport/transport';

const OrderCreated = topic('orders.created', { id: Uuid });
const TRACEPARENT = '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01';

function check(name: string) {
  return { feature: 'typescript/event-messaging', requirement: 'event-trace-context', check: name };
}

function recordingTransport(): Transport & { published: Envelope[] } {
  const published: Envelope[] = [];
  return {
    published,
    async publish(_topic: string, envelope: Envelope) {
      published.push(envelope);
    },
    async subscribe() {},
    async start() {},
    async stop() {},
  };
}

async function waitUntil(condition: () => boolean, timeoutMs = 1000): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) throw new Error(`Timed out after ${timeoutMs}ms`);
    await Bun.sleep(1);
  }
}

describe('event trace context', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    clearTransport();
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    clearTransport();
    setCollector(undefined);
  });

  specTest(
    'a publish inside a span writes the span trace context into the event attributes',
    check('a-publish-inside-a-span-writes-the-trace-attributes'),
    async () => {
      const transport = recordingTransport();
      setTransport(transport);
      const request = startTelemetrySpan({ name: 'POST /orders', kind: 'server' });
      await request.run(() =>
        getPublisher(OrderCreated)(
          { id: crypto.randomUUID() },
          { attributes: { tracestate: 'stale=1', region: 'eu' } },
        ),
      );
      request.finish();

      const { traceId, spanId, traceFlags } = request.context;
      expect(transport.published[0]?.attributes).toEqual({
        region: 'eu',
        [ATTR_EVENT_TRACEPARENT]: `00-${traceId}-${spanId}-${traceFlags}`,
      });
    },
  );

  specTest(
    'a publish outside a span writes no trace attribute',
    check('a-publish-outside-a-span-writes-no-trace-attribute'),
    async () => {
      const transport = recordingTransport();
      setTransport(transport);
      await getPublisher(OrderCreated)({ id: crypto.randomUUID() }, { attributes: { region: 'eu' } });
      expect(transport.published[0]?.attributes).toEqual({ region: 'eu' });
    },
  );

  specTest(
    'the consumer span continues the publisher trace and holds the handler',
    check('the-consumer-span-continues-the-publishers-trace'),
    async () => {
      const broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();
      setTransport(broker);
      const handled: Array<TelemetryTraceContext | undefined> = [];
      await broker.subscribe(
        handler(OrderCreated).handle(async () => {}),
        async (_msg: Message<unknown>) => {
          handled.push(tryContext<{ __putnamiTrace?: TelemetryTraceContext }>()?.__putnamiTrace);
        },
      );

      const request = startTelemetrySpan({ name: 'POST /orders', kind: 'server' });
      await request.run(() => getPublisher(OrderCreated)({ id: crypto.randomUUID() }));
      request.finish();
      await waitUntil(() => handled.length === 1);
      await broker.stop();

      const consumer = handled[0];
      expect(consumer?.traceId).toBe(request.context.traceId);
      expect(consumer?.spanId).not.toBe(request.context.spanId);
      const recorded = collector.drainSpans().find((span) => span.name === 'process orders.created');
      expect(recorded?.spanId).toBe(consumer?.spanId);
      expect(recorded?.parentSpanId).toBe(request.context.spanId);
      expect(recorded?.kind).toBe(5);
    },
  );

  specTest(
    'an event without trace attributes starts a root, whatever span delivers it',
    check('an-event-without-trace-attributes-starts-a-root'),
    async () => {
      const broker = new MemoryBroker({ simulateDuplicates: false });
      await broker.start();
      const handled: Array<TelemetryTraceContext | undefined> = [];
      await broker.subscribe(
        handler(OrderCreated).handle(async () => {}),
        async () => {
          handled.push(tryContext<{ __putnamiTrace?: TelemetryTraceContext }>()?.__putnamiTrace);
        },
      );
      const delivering = startTelemetrySpan({ name: 'POST /push', kind: 'server' });
      await delivering.run(() =>
        broker.publish('orders.created', {
          protocol: PUTNAMI_EVENTS_PROTOCOL,
          id: 'evt-root',
          topic: 'orders.created',
          payload: { id: crypto.randomUUID() },
          timestamp: new Date().toISOString(),
          attributes: {},
          attempt: 1,
        }),
      );
      await waitUntil(() => handled.length === 1);
      await broker.stop();
      delivering.finish();

      const consumer = collector.drainSpans().find((span) => span.name === 'process orders.created');
      expect(consumer?.parentSpanId).toBeUndefined();
      expect(consumer?.traceId).not.toBe(delivering.context.traceId);
      expect(handled[0]?.traceId).toBe(consumer?.traceId);
    },
  );

  specTest(
    'the managed publisher sends the trace attributes as the W3C headers, outside the frame',
    check('the-managed-publisher-sends-the-trace-attributes-as-headers'),
    async () => {
      const calls: RequestInit[] = [];
      const transport = eventServerTransport({
        contractVersion: 1,
        endpoint: 'https://events.example',
        audience: 'https://events.example',
        protocol: PUTNAMI_EVENTS_PROTOCOL,
        tokenSource: async () => 'token',
        retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
        fetch: async (_input, init) => {
          calls.push(init ?? {});
          return Response.json({
            protocol: PUTNAMI_EVENTS_PROTOCOL,
            id: 'evt-1',
            topic: 'orders.created',
            timestamp: new Date().toISOString(),
          });
        },
      });
      const attributes = { region: 'eu', [ATTR_EVENT_TRACEPARENT]: TRACEPARENT, [ATTR_EVENT_TRACESTATE]: 'vendor=v' };
      const envelope: Envelope = {
        protocol: PUTNAMI_EVENTS_PROTOCOL,
        id: 'evt-1',
        topic: 'orders.created',
        topicVersion: '1',
        payload: {},
        timestamp: new Date().toISOString(),
        attributes,
        attempt: 1,
      };
      await transport.publish('orders.created', envelope);
      await transport.publish('orders.created', envelope, { traceparent: 'explicit-wins' });

      const first = new Headers(calls[0]?.headers);
      expect(first.get('traceparent')).toBe(TRACEPARENT);
      expect(first.get('tracestate')).toBe('vendor=v');
      expect(JSON.parse(String(calls[0]?.body)).attributes).toEqual({ region: 'eu' });
      const second = new Headers(calls[1]?.headers);
      expect(second.get('traceparent')).toBe('explicit-wins');
      expect(second.get('tracestate')).toBeNull();
      expect(envelope.attributes).toBe(attributes);
      expect(Object.keys(attributes)).toHaveLength(3);
    },
  );

  it('a failed handler ends its consumer span with the failure message', async () => {
    const broker = new MemoryBroker({ simulateDuplicates: false });
    await broker.start();
    let calls = 0;
    await broker.subscribe(
      handler(OrderCreated).handle(async () => {}),
      async () => {
        calls += 1;
        throw new Error('boom');
      },
    );
    await broker.publish('orders.created', {
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      id: 'evt-fail',
      topic: 'orders.created',
      payload: { id: crypto.randomUUID() },
      timestamp: new Date().toISOString(),
      attributes: { [ATTR_EVENT_TRACEPARENT]: TRACEPARENT },
      attempt: 1,
    });
    await waitUntil(() => calls >= 1);
    await Bun.sleep(5);
    await broker.stop();

    const failed = collector.drainSpans().find((span) => span.name === 'process orders.created');
    expect(failed?.traceId).toBe('4bf92f3577b34da6a3ce929d0e0e4736');
    expect(failed?.parentSpanId).toBe('00f067aa0ba902b7');
    expect(failed?.statusMessage).toBe(EVENT_CONSUMER_FAILURE_MESSAGE);
  });

  it('a push adopts the trace context the provider pushed when the envelope carries none', () => {
    const decode = (envAttributes: Record<string, string>, pushed: Record<string, string>) => {
      const data = Buffer.from(
        JSON.stringify({
          protocol: PUTNAMI_EVENTS_PROTOCOL,
          id: 'evt-1',
          topic: 'orders.created',
          payload: {},
          attributes: envAttributes,
        }),
      ).toString('base64');
      return decodePushEnvelope({
        message: { data, attributes: pushed },
        subscription: 'projects/p/subscriptions/s',
      } as never)?.attributes;
    };
    expect(decode({ region: 'eu', tracestate: 'stale=1' }, { traceparent: TRACEPARENT })).toEqual({
      region: 'eu',
      traceparent: TRACEPARENT,
    });
    const own = '00-11111111111111111111111111111111-2222222222222222-01';
    expect(decode({ traceparent: own }, { traceparent: TRACEPARENT })?.['traceparent']).toBe(own);
  });

  it('a tracestate option alone keeps the traceparent of the event', async () => {
    const calls: RequestInit[] = [];
    const transport = eventServerTransport({
      contractVersion: 1,
      endpoint: 'https://events.example',
      audience: 'https://events.example',
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      tokenSource: async () => 'token',
      retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
      fetch: async (_input, init) => {
        calls.push(init ?? {});
        return Response.json({
          protocol: PUTNAMI_EVENTS_PROTOCOL,
          id: 'evt-1',
          topic: 'orders.created',
          timestamp: new Date().toISOString(),
        });
      },
    });
    await transport.publish(
      'orders.created',
      {
        protocol: PUTNAMI_EVENTS_PROTOCOL,
        id: 'evt-1',
        topic: 'orders.created',
        topicVersion: '1',
        payload: {},
        timestamp: new Date().toISOString(),
        attributes: { [ATTR_EVENT_TRACEPARENT]: TRACEPARENT },
        attempt: 1,
      },
      { tracestate: 'vendor=option' },
    );
    const headers = new Headers(calls[0]?.headers);
    expect(headers.get('traceparent')).toBe(TRACEPARENT);
    expect(headers.get('tracestate')).toBe('vendor=option');
  });
});
