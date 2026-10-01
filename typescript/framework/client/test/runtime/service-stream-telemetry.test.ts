import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientContractOperation, ClientSchema, ClientTransportProtocol } from '@putnami/application';
import { SERVICE_WEBSOCKET_SUBPROTOCOL, setCollector, TelemetryCollector } from '@putnami/application';
import { BaseClient } from '../../src/runtime/base-client';
import type { StreamObserver } from '../../src/runtime/stream.type';

// A generated stream never traverses the unary interceptor chain, so its call
// measurement comes from the stream session. These tests read what that
// measurement exports, and what the provider received as its parent, from a
// real socket on each declared transport.

const WATCH_PATH = '/items/watch';
const FEATURE = 'typescript/service-clients';

const output = {
  type: 'object',
  properties: { value: { type: 'string' } },
  required: ['value'],
  additionalProperties: false,
} as const satisfies ClientSchema;

interface Received {
  /** The traceparent each SSE request carried, in arrival order. */
  readonly sse: (string | null)[];
  /** The propagation context each WebSocket init frame carried. */
  readonly inits: Record<string, unknown>[];
}

const servers: (() => void)[] = [];
let collector: TelemetryCollector;

beforeEach(() => {
  collector = new TelemetryCollector();
  setCollector(collector);
});

afterEach(() => {
  setCollector(undefined);
  for (const stop of servers.splice(0)) stop();
});

/** A provider serving one path over SSE and the first-party WebSocket wire. */
function provider(options: { sseStatus?: number } = {}): { url: string; received: Received } {
  const received: Received = { sse: [], inits: [] };
  const server = Bun.serve<Record<string, never>, Record<string, never>>({
    port: 0,
    fetch(request, target) {
      if (request.headers.get('upgrade')?.toLowerCase() !== 'websocket') {
        received.sse.push(request.headers.get('traceparent'));
        if (options.sseStatus !== undefined) {
          return Response.json({ code: 'not_found', message: 'no' }, { status: options.sseStatus });
        }
        return new Response('id: 1\ndata: {"value":"a"}\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
      }
      const upgraded = target.upgrade(request, {
        data: {},
        headers: { 'Sec-WebSocket-Protocol': SERVICE_WEBSOCKET_SUBPROTOCOL },
      });
      return upgraded ? undefined : new Response('expected a websocket upgrade', { status: 400 });
    },
    websocket: {
      message(socket, message) {
        const frame = JSON.parse(String(message)) as Record<string, unknown>;
        if (frame['type'] !== 'init') return;
        received.inits.push((frame['context'] as Record<string, unknown> | undefined) ?? {});
        socket.send(JSON.stringify({ v: 1, type: 'ready', resumed: false }));
        socket.send(
          JSON.stringify({
            v: 1,
            type: 'message',
            sequence: '1',
            payload: { encoding: 'json', value: { value: 'a' } },
          }),
        );
        socket.send(JSON.stringify({ v: 1, type: 'result' }));
      },
    },
  });
  servers.push(() => server.stop(true));
  return { url: `http://localhost:${server.port}`, received };
}

function watchOperation(order: readonly ClientTransportProtocol[]): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: order.map((protocol) =>
      protocol === 'websocket'
        ? {
            protocol,
            path: WATCH_PATH,
            encoding: 'json' as const,
            websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
          }
        : { protocol, path: WATCH_PATH, encoding: 'json' as const },
    ),
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'safe' },
  };
}

class WatchClient extends BaseClient {
  readonly serviceName = 'catalog.items';

  watch(): StreamObserver<{ value: string }> {
    return this.serviceStream<{ value: string }>('GET', WATCH_PATH, { operationId: 'watchItems' });
  }
}

function watch(url: string, order: readonly ClientTransportProtocol[]): Promise<unknown[]> {
  const client = new WatchClient({
    baseUrl: url,
    transport: 'http',
    serviceId: 'catalog.items',
    operationContracts: { watchItems: watchOperation(order) },
    streamInterceptors: [
      async (request, next) => {
        request.headers.set('X-Client-Id', 'consumer.workload');
        return next(request);
      },
    ],
  });
  return new Promise((resolve, reject) => {
    const values: unknown[] = [];
    const stream = client.watch();
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

/** The call series one stream exported, with the attributes that label it. */
function calls() {
  return collector
    .drainAll()
    .flatMap((bucket) => bucket.counterSeries ?? [])
    .filter((series) => series.name === 'rpc.client.calls');
}

const LABELS = { 'rpc.system': 'putnami', 'rpc.service': 'catalog.items', 'rpc.method': 'watchItems' };

describe('the call measurement of a generated stream', () => {
  specTest(
    'is the parent the provider receives, on SSE and on the WebSocket init frame',
    {
      feature: FEATURE,
      requirement: 'stream-call-telemetry',
      check: 'a-stream-call-span-is-the-parent-the-provider-receives',
    },
    async () => {
      for (const protocol of ['sse', 'websocket'] as const) {
        const { url, received } = provider();
        expect(await watch(url, [protocol])).toEqual([{ value: 'a' }]);
        const spans = collector.drainSpans();
        expect(spans).toHaveLength(1);
        const call = spans[0];
        expect(call).toMatchObject({ name: 'watchItems', kind: 3, statusCode: 1 });
        const traceparent = `00-${call?.traceId}-${call?.spanId}-01`;
        if (protocol === 'sse') expect(received.sse).toEqual([traceparent]);
        else expect(received.inits).toEqual([{ traceparent }]);
      }
    },
  );

  specTest(
    'is labelled with the transport that carried it and the status it was admitted with',
    {
      feature: FEATURE,
      requirement: 'stream-call-telemetry',
      check: 'a-stream-is-measured-under-the-transport-that-carried-it',
    },
    async () => {
      // SSE is admitted by its response head, so the call carries its status;
      // a WebSocket session is admitted in band and carries none.
      expect(await watch(provider().url, ['sse'])).toEqual([{ value: 'a' }]);
      expect(calls()).toEqual([
        {
          name: 'rpc.client.calls',
          value: 1,
          attributes: { ...LABELS, 'network.protocol.name': 'sse', 'http.response.status_code': 200 },
        },
      ]);

      // SSE declared first and not served here: the stream falls back to the
      // WebSocket wire. The session SSE never admitted is measured as the
      // refusal it was; the one that carried the stream is measured under its
      // own protocol, not under the first declared one.
      const { url, received } = provider({ sseStatus: 404 });
      expect(await watch(url, ['sse', 'websocket'])).toEqual([{ value: 'a' }]);
      expect(received.sse).toHaveLength(1);
      expect(received.inits).toHaveLength(1);
      expect(calls()).toEqual(
        expect.arrayContaining([
          {
            name: 'rpc.client.calls',
            value: 1,
            attributes: {
              ...LABELS,
              'network.protocol.name': 'sse',
              'http.response.status_code': 404,
              'error.type': expect.any(String),
            },
          },
          { name: 'rpc.client.calls', value: 1, attributes: { ...LABELS, 'network.protocol.name': 'websocket' } },
        ]),
      );
    },
  );
});
