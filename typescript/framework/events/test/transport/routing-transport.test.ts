import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { handler } from '../../src/handler/handler';
import type { HandlerDefinition } from '../../src/handler/handler';
import type { Message } from '../../src/topic/message';
import { topic } from '../../src/topic/topic';
import { routingTransport, type Transport } from '../../src/transport';
import { buildEnvelope } from '../../src/transport/transport';

const AnalyticsTopic = topic('analytics.page_view', { id: Uuid }, { channel: 'analytics' });
const StreamTopic = topic('stream.order_updated', { id: Uuid });
const OtherTopic = topic('other.event', { id: Uuid });

describe('RoutingTransport', () => {
  it('routes by topic channel before topic name matches', async () => {
    const stream = new FakeTransport();
    const analytics = new FakeTransport();
    const transport = routingTransport({
      transports: { stream, analytics },
      routes: [
        { match: 'analytics.*', transport: 'stream' },
        { channel: 'analytics', transport: 'analytics' },
      ],
      defaultTransport: 'stream',
    });

    await transport.publish(AnalyticsTopic.name, buildEnvelope(AnalyticsTopic, { id: crypto.randomUUID() }));
    await transport.subscribe(
      handler(AnalyticsTopic).handle(async () => {}),
      async () => {},
    );

    expect(analytics.published).toEqual(['analytics.page_view']);
    expect(analytics.subscribed).toEqual(['analytics.page_view']);
    expect(stream.published).toEqual([]);
    expect(stream.subscribed).toEqual([]);
  });

  it('falls back to topic name matches and default transport', async () => {
    const stream = new FakeTransport();
    const fallback = new FakeTransport();
    const transport = routingTransport({
      transports: { stream, fallback },
      routes: [{ match: 'stream.*', transport: 'stream' }],
      defaultTransport: 'fallback',
    });

    await transport.publish(StreamTopic.name, buildEnvelope(StreamTopic, { id: crypto.randomUUID() }));
    await transport.publish(OtherTopic.name, buildEnvelope(OtherTopic, { id: crypto.randomUUID() }));

    expect(stream.published).toEqual(['stream.order_updated']);
    expect(fallback.published).toEqual(['other.event']);
  });

  it('fans out when a route targets multiple transports', async () => {
    const primary = new FakeTransport();
    const mirror = new FakeTransport();
    const transport = routingTransport({
      transports: { primary, mirror },
      routes: [{ match: 'analytics.*', transport: ['primary', 'mirror'] }],
    });

    await transport.publish(AnalyticsTopic.name, buildEnvelope(AnalyticsTopic, { id: crypto.randomUUID() }));

    expect(primary.published).toEqual(['analytics.page_view']);
    expect(mirror.published).toEqual(['analytics.page_view']);
  });

  it('fails when no route or default transport matches', async () => {
    const transport = routingTransport({
      transports: { stream: new FakeTransport() },
      routes: [{ match: 'stream.*', transport: 'stream' }],
    });

    await expect(
      transport.publish(OtherTopic.name, buildEnvelope(OtherTopic, { id: crypto.randomUUID() })),
    ).rejects.toThrow("No event transport route matched topic 'other.event'.");
  });

  it('validates named transport references', () => {
    expect(() =>
      routingTransport({
        transports: { stream: new FakeTransport() },
        routes: [{ channel: 'analytics', transport: 'missing' }],
      }),
    ).toThrow("Unknown event transport 'missing'.");
  });
});

class FakeTransport implements Transport {
  readonly published: string[] = [];
  readonly subscribed: string[] = [];
  started = false;
  stopped = false;

  async publish(topic: string): Promise<void> {
    this.published.push(topic);
  }

  async subscribe(
    definition: HandlerDefinition,
    _callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    this.subscribed.push(definition.topic.name);
  }

  async start(): Promise<void> {
    this.started = true;
  }

  async stop(): Promise<void> {
    this.stopped = true;
  }
}
