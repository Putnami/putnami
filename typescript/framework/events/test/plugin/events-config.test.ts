import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '@putnami/application';
import { resetConfigLoader, Uuid } from '@putnami/runtime';
import { EventsRuntimeConfig } from '../../src/events.config';
import { EventsPlugin } from '../../src/events.plugin';
import { getPublisher, getTransport } from '../../src/publisher/publisher';
import { PUTNAMI_EVENTS_PROTOCOL } from '../../src/protocol';
import type { Message } from '../../src/topic/message';
import { handler, type HandlerDefinition } from '../../src/handler';
import { topic } from '../../src/topic/topic';
import { EventServerPublisherTransport } from '../../src/transport/event-server.transport';
import type { Transport } from '../../src/transport/transport';

const ManagedTopic = topic('managed.created', { id: Uuid }, { version: '1' });

describe('events managed runtime config', () => {
  beforeEach(() => {
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  afterEach(() => {
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  it('contributes the framework-owned events config block', () => {
    const plugin = new EventsPlugin({ autoScan: false });
    expect(plugin.configDefinitions()).toEqual([EventsRuntimeConfig]);
  });

  it('selects eventserver from the managed document and preserves the code token source', async () => {
    process.env.CONFIG_DATA = `
events:
  transport: eventserver
  eventServer:
    contractVersion: 1
    endpoint: https://events.example
    audience: https://events.example
    protocol: putnami.events.v1
    workspaceId: workspace-1
    environment: production
    workload: orders-api
    topologyGenerationId: generation-1
`;
    resetConfigLoader();

    const audiences: string[] = [];
    const bodies: Record<string, unknown>[] = [];
    const plugin = new EventsPlugin({
      autoScan: false,
      eventServer: {
        tokenSource: async ({ audience }) => {
          audiences.push(audience);
          return 'token';
        },
        fetch: async (_input, init) => {
          const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
          bodies.push(body);
          return Response.json({
            protocol: PUTNAMI_EVENTS_PROTOCOL,
            id: body['id'],
            topic: body['topic'],
            timestamp: '2026-07-22T00:00:00.000Z',
          });
        },
        retry: { maxAttempts: 1 },
      },
    });

    try {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.warmup({} as any);
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.start({} as any);
      await getPublisher(ManagedTopic)({ id: crypto.randomUUID() });

      expect(getTransport()).toBeInstanceOf(EventServerPublisherTransport);
      expect(audiences).toEqual(['https://events.example']);
      expect(bodies).toHaveLength(1);
      expect(bodies[0]?.['channel']).toBeUndefined();
      expect(bodies[0]?.['workspaceId']).toBeUndefined();
    } finally {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.stop({} as any);
    }
  });

  it('keeps an explicit direct transport compatible when a string selector is present', async () => {
    process.env.CONFIG_DATA = 'events:\n  transport: eventserver\n';
    resetConfigLoader();
    const direct = new RecordingTransport();
    const plugin = new EventsPlugin({ autoScan: false, transport: direct });

    try {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.warmup({} as any);
      expect(getTransport()).toBe(direct);
    } finally {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.stop({} as any);
    }
  });

  it('mounts document-configured disabled push bootstrap without dispatching', async () => {
    process.env.CONFIG_DATA = `
events:
  delivery: push
  push:
    enabled: false
`;
    resetConfigLoader();
    let called = false;
    let receiver: ((ctx: HttpRequestContext) => Promise<{ status?: number }>) | undefined;
    const post = (
      _path: string,
      route: (ctx: HttpRequestContext) => Promise<{ status?: number }>,
      _options: unknown,
    ) => {
      receiver = route;
    };
    const app = { ensurePlugin: async () => ({ post }) };
    const direct = new RecordingTransport();
    const plugin = new EventsPlugin({
      autoScan: false,
      transport: direct,
      handlers: [
        handler(ManagedTopic).handle(async () => {
          called = true;
        }),
      ],
    });

    try {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.warmup(app as any);
      expect(receiver).toBeDefined();
      const response = await receiver?.({ headers: new Headers() } as HttpRequestContext);
      expect(response?.status).toBe(503);
      expect(called).toBe(false);
      expect(direct.subscribed).toEqual([]);
    } finally {
      // biome-ignore lint/suspicious/noExplicitAny: lifecycle test stub
      await plugin.stop(app as any);
    }
  });
});

class RecordingTransport implements Transport {
  readonly subscribed: string[] = [];

  async publish(): Promise<void> {}

  async subscribe(
    definition: HandlerDefinition,
    _callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    this.subscribed.push(definition.topic.name);
  }

  async start(): Promise<void> {}

  async stop(): Promise<void> {}
}
