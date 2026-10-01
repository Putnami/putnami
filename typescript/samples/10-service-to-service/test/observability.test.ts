/**
 * TS→TS observability cell of the cross-language interop matrix: a TypeScript consumer calling
 * the TypeScript provider of this sample. A real inbound request carrying W3C
 * trace context reaches a consumer route, which calls the provider through the
 * generated clients: over Connect and REST JSON, over each declared stream
 * shape, and with a raw octet payload. The assertions read what the telemetry
 * plugin actually collected, the way an OTLP pipeline receives it.
 *
 * The provider is the same application `putnami serve` runs, started on a
 * reserved port with the CONFIG_DATA binding test/client.test.ts uses. Only the
 * consumer enables telemetry, so every collected span and series below belongs
 * to the consumer side of the boundary.
 */
import { type Application, application, getCollector, http, telemetry } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { afterAll, beforeAll, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  BlobsClient,
  ItemsClient,
  QuotesClient,
  registerBlobsClient,
  registerItemsClient,
  registerQuotesClient,
} from '../clients/ts/src';
import { SAMPLE_TENANT } from '../src/caller-identity';
import { app as createApp } from '../src/main';
import { SEEDED_ITEM_IDS } from '../src/store';
import { CATALOG_API_KEY } from '../src/workload-identity';

/** The trace the consumer's inbound request carries, and the span that sent it. */
const INCOMING_TRACE_ID = '33333333333333333333333333333333';
const INCOMING_SPAN_ID = '4444444444444444';

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('service-to-service observability', () => {
  let provider: Application;
  let providerUrl: string;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    // The provider is its own consumer on `/proxy`, so its configuration must
    // carry its own base URL before it starts: reserve a port, release it, and
    // hand the same number to the binding and to the server.
    const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
    const port = reservation.port;
    reservation.stop(true);
    providerUrl = `http://localhost:${port}`;
    bindTo(providerUrl);
    provider = createApp({ port });
    await provider.start();
  });

  afterAll(async () => {
    await provider.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  specTest(
    'continues an incoming trace into both wires, measures each call once and exports no secret',
    {
      feature: FEATURE,
      requirement: 'a-call-is-observable-and-ends-with-its-application',
      check: 'a-generated-call-continues-the-inbound-trace-and-is-measured-once',
    },
    async () => {
      // A recording proxy in front of the provider shows what each generated call
      // put on the wire, so the trace context the provider received is read from
      // the request itself rather than assumed from the consumer's own spans.
      const wire = startWireRecorder(providerUrl);
      bindTo(wire.url);
      let consumer: Application;
      // The consumer route writes no header and starts no span of its own: it
      // calls the generated clients with the context of the request in flight.
      const consumerHttp = http({ port: 0 }).get('/quote-and-items', async () => {
        const quote = await consumer.context.get(QuotesClient).getQuotes_id({ path: { id: '1' } });
        const list = await consumer.context.get(ItemsClient).getItems({
          query: { search: 'et', limit: 10 },
          headers: { 'x-catalog-tenant': SAMPLE_TENANT },
        });
        return { quote: quote.id, items: list.items.map((item) => item.id) };
      });
      consumer = application()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'items-consumer-tests' }))
        .use(consumerHttp);
      registerItemsClient(consumer);
      registerQuotesClient(consumer);
      await consumer.start();

      try {
        // Whatever the provider's own start-up recorded belongs to no call under
        // test, so the collector starts this scenario empty.
        const collector = getCollector();
        collector?.drainAll();
        collector?.drainSpans();

        const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/quote-and-items`, {
          headers: { traceparent: `00-${INCOMING_TRACE_ID}-${INCOMING_SPAN_ID}-01` },
        });
        expect(response.status).toBe(200);
        const body = (await response.json()) as { quote: string; items: string[] };
        expect(body.quote).toBe('1');
        // The store is shared by every test file of this process. Each file removes
        // what it creates, and counting the seeded items alone keeps this assertion
        // independent of file order even when one does not.
        expect(body.items.filter((id) => SEEDED_ITEM_IDS.includes(id))).toEqual(['item-1', 'item-2']);

        const spans = collector?.drainSpans() ?? [];
        const server = spans.find((span) => span.kind === 2 && span.name.includes('/quote-and-items'));
        // The inbound request is the root of everything below: its span continues
        // the caller's trace instead of starting a new one.
        expect(server).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: INCOMING_SPAN_ID });

        for (const [operation, path] of [
          ['getQuotes_id', '/catalog.items.v1.QuotesService/GetQuotesById'],
          ['getItems', '/items'],
        ] as const) {
          const call = spans.find((span) => span.name === operation);
          const attempt = spans.find((span) => span.name === `${operation} attempt`);
          expect(call).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: server?.spanId });
          expect(attempt).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: call?.spanId });
          // The provider receives the attempt span as its parent, on either wire,
          // so the trace joins across the call instead of restarting at it.
          expect(wire.requests.find((request) => request.path === path)?.traceparent).toBe(
            `00-${INCOMING_TRACE_ID}-${attempt?.spanId}-01`,
          );
        }

        const buckets = collector?.drainAll() ?? [];
        const calls = buckets
          .flatMap((bucket) => bucket.counterSeries ?? [])
          .filter((series) => series.name === 'rpc.client.calls');
        // One series per operation, each counted once, and the exact attribute
        // set: every value here is bounded by the contract, so no identifier,
        // query or payload can widen the label vocabulary.
        const labels = { 'rpc.service': 'catalog.items', 'rpc.system': 'putnami', 'http.response.status_code': 200 };
        expect(calls).toHaveLength(2);
        expect(calls).toEqual(
          expect.arrayContaining([
            {
              name: 'rpc.client.calls',
              value: 1,
              attributes: { ...labels, 'network.protocol.name': 'connect', 'rpc.method': 'getQuotes_id' },
            },
            {
              name: 'rpc.client.calls',
              value: 1,
              attributes: { ...labels, 'network.protocol.name': 'rest-json', 'rpc.method': 'getItems' },
            },
          ]),
        );

        // Nothing a consumer sent and nothing a provider answered reaches an
        // export: not the injected credential, not the query string, not an item
        // name, not the tenant the route carried.
        const exported = JSON.stringify({ spans, buckets }, (_key, value) =>
          typeof value === 'bigint' ? value.toString() : value,
        );
        for (const forbidden of [CATALOG_API_KEY, 'search=', 'Widget', 'Gadget', SAMPLE_TENANT]) {
          expect(exported).not.toContain(forbidden);
        }
      } finally {
        await consumer.stop();
        wire.stop();
        bindTo(providerUrl);
      }
    },
  );

  specTest(
    'measures each generated stream once at its terminal, under the transport that carried it',
    {
      feature: FEATURE,
      requirement: 'a-call-is-observable-and-ends-with-its-application',
      check: 'a-generated-stream-is-one-traced-call-measured-at-its-terminal',
    },
    async () => {
      // The four stream shapes this provider declares, opened inside one traced
      // inbound request, plus one conversation the provider ends with its
      // declared error. A stream is one call however many messages it carries,
      // so each must be one span and one count, finished at its terminal.
      let consumer: Application;
      const consumerHttp = http({ port: 0 }).get('/streams', async () => {
        const items = consumer.context.get(ItemsClient);
        const watched = await drain(items.getItems_idWatch({ path: { id: 'item-1' }, query: { follow: false } }));
        const revisions = await drain(items.getItems_idHistory({ path: { id: 'item-1' } }));
        const adjust = items.getItems_idAdjust({ path: { id: 'item-1' } });
        const adjusted = drain(adjust);
        adjust.send({ delta: 5 });
        adjust.end();
        const negotiate = items.getItems_idNegotiate({ path: { id: 'item-1' } });
        const negotiated = drain(negotiate);
        negotiate.send({ delta: 4 });
        negotiate.end();
        const missing = items.getItems_idAdjust({ path: { id: 'missing' } });
        const refused = drain(missing).catch((error: { code?: string }) => error.code);
        missing.end();
        return {
          watched: watched.length,
          revisions: revisions.length,
          adjusted: (await adjusted).length,
          negotiated: (await negotiated).length,
          refused: await refused,
        };
      });
      consumer = application()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'items-consumer-tests' }))
        .use(consumerHttp);
      registerItemsClient(consumer);
      await consumer.start();

      try {
        const collector = getCollector();
        collector?.drainAll();
        collector?.drainSpans();

        const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/streams`, {
          headers: { traceparent: `00-${INCOMING_TRACE_ID}-${INCOMING_SPAN_ID}-01` },
        });
        expect(await response.json()).toEqual({
          watched: 1,
          revisions: 3,
          adjusted: 1,
          negotiated: 2,
          refused: 'not_found',
        });

        const spans = collector?.drainSpans() ?? [];
        const server = spans.find((span) => span.kind === 2 && span.name.includes('/streams'));
        expect(server).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: INCOMING_SPAN_ID });
        const streams = ['getItems_idWatch', 'getItems_idHistory', 'getItems_idNegotiate'];
        for (const operation of streams) {
          // One client span per stream, a child of the inbound request, ended
          // successfully at the stream's own terminal.
          const calls = spans.filter((span) => span.name === operation);
          expect(calls).toHaveLength(1);
          expect(calls[0]).toMatchObject({
            kind: 3,
            traceId: INCOMING_TRACE_ID,
            parentSpanId: server?.spanId,
            statusCode: 1,
          });
        }
        // The two client streams: one ended by its result, one by the declared
        // error, which is the span's terminal code.
        const adjusts = spans.filter((span) => span.name === 'getItems_idAdjust');
        expect(adjusts.map((span) => [span.parentSpanId, span.statusCode, span.statusMessage ?? '']).sort()).toEqual([
          [server?.spanId, 1, ''],
          [server?.spanId, 2, 'not_found'],
        ]);

        const buckets = collector?.drainAll() ?? [];
        const labels = { 'rpc.service': 'catalog.items', 'rpc.system': 'putnami' };
        const calls = buckets
          .flatMap((bucket) => bucket.counterSeries ?? [])
          .filter((series) => series.name === 'rpc.client.calls');
        // Counted once each, at the terminal, under the transport that carried
        // it: SSE states the status it was admitted with, a WebSocket session
        // is admitted in band and states none, and the refused conversation
        // carries its declared code.
        expect(calls).toHaveLength(5);
        expect(calls).toEqual(
          expect.arrayContaining([
            {
              name: 'rpc.client.calls',
              value: 1,
              attributes: {
                ...labels,
                'network.protocol.name': 'sse',
                'rpc.method': 'getItems_idWatch',
                'http.response.status_code': 200,
              },
            },
            ...['getItems_idHistory', 'getItems_idAdjust', 'getItems_idNegotiate'].map((method) => ({
              name: 'rpc.client.calls',
              value: 1,
              attributes: { ...labels, 'network.protocol.name': 'websocket', 'rpc.method': method },
            })),
            {
              name: 'rpc.client.calls',
              value: 1,
              attributes: {
                ...labels,
                'network.protocol.name': 'websocket',
                'rpc.method': 'getItems_idAdjust',
                'http.response.status_code': 404,
                'error.type': 'not_found',
              },
            },
          ]),
        );

        const exported = JSON.stringify({ spans, buckets }, (_key, value) =>
          typeof value === 'bigint' ? value.toString() : value,
        );
        for (const forbidden of [CATALOG_API_KEY, 'follow=', 'Widget', 'item-1', 'missing']) {
          expect(exported).not.toContain(forbidden);
        }
        expectBoundedLabels(buckets);
      } finally {
        await consumer.stop();
      }
    },
  );

  specTest(
    'traces a raw octet call and measures it once without exporting its octets',
    {
      feature: FEATURE,
      requirement: 'a-call-is-observable-and-ends-with-its-application',
      check: 'a-raw-octet-call-is-traced-and-measured-without-its-octets',
    },
    async () => {
      const wire = startWireRecorder(providerUrl);
      bindTo(wire.url);
      let consumer: Application;
      const consumerHttp = http({ port: 0 }).get('/echo', async () => {
        const echoed = await consumer.context.get(BlobsClient).postBlobsEcho({ body: OCTET_MARKER });
        return { bytes: echoed.body.byteLength, same: Buffer.from(echoed.body).equals(Buffer.from(OCTET_MARKER)) };
      });
      consumer = application()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'items-consumer-tests' }))
        .use(consumerHttp);
      registerBlobsClient(consumer);
      await consumer.start();

      try {
        const collector = getCollector();
        collector?.drainAll();
        collector?.drainSpans();

        const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/echo`, {
          headers: { traceparent: `00-${INCOMING_TRACE_ID}-${INCOMING_SPAN_ID}-01` },
        });
        expect(await response.json()).toEqual({ bytes: OCTET_MARKER.byteLength, same: true });

        const spans = collector?.drainSpans() ?? [];
        const server = spans.find((span) => span.kind === 2 && span.name.includes('/echo'));
        const call = spans.find((span) => span.name === 'postBlobsEcho');
        const attempt = spans.find((span) => span.name === 'postBlobsEcho attempt');
        expect(call).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: server?.spanId, statusCode: 1 });
        expect(attempt).toMatchObject({ traceId: INCOMING_TRACE_ID, parentSpanId: call?.spanId, statusCode: 1 });
        // The octets travel with the attempt span as their parent, like any call.
        expect(wire.requests.find((request) => request.path === '/blobs/echo')?.traceparent).toBe(
          `00-${INCOMING_TRACE_ID}-${attempt?.spanId}-01`,
        );

        const buckets = collector?.drainAll() ?? [];
        const calls = buckets
          .flatMap((bucket) => bucket.counterSeries ?? [])
          .filter((series) => series.name === 'rpc.client.calls');
        expect(calls).toEqual([
          {
            name: 'rpc.client.calls',
            value: 1,
            attributes: {
              'rpc.service': 'catalog.items',
              'rpc.system': 'putnami',
              'rpc.method': 'postBlobsEcho',
              'network.protocol.name': 'rest-json',
              'http.response.status_code': 200,
            },
          },
        ]);

        // Neither the octets nor any rendering of them — text, hex, base64 —
        // reaches an export.
        const exported = JSON.stringify({ spans, buckets }, (_key, value) =>
          typeof value === 'bigint' ? value.toString() : value,
        );
        const marker = Buffer.from(OCTET_MARKER);
        for (const forbidden of [
          CATALOG_API_KEY,
          marker.toString('latin1'),
          marker.toString('hex'),
          marker.toString('base64'),
        ]) {
          expect(exported).not.toContain(forbidden);
        }
        expectBoundedLabels(buckets);
      } finally {
        await consumer.stop();
        wire.stop();
        bindTo(providerUrl);
      }
    },
  );
});

/**
 * A payload no telemetry field could carry by accident: readable text after a
 * run of octets no text or JSON pipeline passes through unchanged.
 */
const OCTET_MARKER = new Uint8Array([0x00, 0xff, 0xfe, 0x80, ...new TextEncoder().encode('octet-marker-3460')]);

/**
 * The whole label vocabulary a generated call may export. Every value is
 * bounded by the contract; the attempt ordinal is bounded by the declared
 * attempt cap.
 */
const CLIENT_METRIC_KEYS = new Set([
  'rpc.system',
  'rpc.service',
  'rpc.method',
  'network.protocol.name',
  'http.response.status_code',
  'error.type',
  'rpc.client.attempt',
]);

/** Every client series carries only labels the contract bounds. */
function expectBoundedLabels(buckets: ReturnType<NonNullable<ReturnType<typeof getCollector>>['drainAll']>): void {
  for (const series of buckets.flatMap((bucket) => [
    ...(bucket.counterSeries ?? []),
    ...(bucket.histogramSeries ?? []),
  ])) {
    if (!series.name.startsWith('rpc.client.')) continue;
    for (const key of Object.keys(series.attributes)) {
      expect({ series: series.name, key, bounded: CLIENT_METRIC_KEYS.has(key) }).toEqual({
        series: series.name,
        key,
        bounded: true,
      });
    }
  }
}

/** Read a stream to its terminal: its values on completion, its error otherwise. */
function drain<T>(stream: {
  onMessage(handler: (value: T) => void): unknown;
  onError(handler: (error: Error) => void): unknown;
  onComplete(handler: () => void): unknown;
}): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

/** Point the process configuration at `url` with the credential both wires need. */
function bindTo(url: string): void {
  process.env.CONFIG_DATA = JSON.stringify({
    clients: {
      clientId: 'service-to-service-sample',
      services: {
        'catalog.items': {
          url,
          allowInsecure: true,
          credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
        },
      },
    },
  });
  resetConfigLoader();
}

/** What one request carried to the provider. A credential value is never recorded. */
interface WireRequest {
  path: string;
  traceparent: string | null;
}

/**
 * A recording proxy in front of the provider. It forwards every request and
 * response unchanged, and logs the trace context each request carried.
 */
function startWireRecorder(target: string) {
  const requests: WireRequest[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      requests.push({ path: url.pathname, traceparent: request.headers.get('traceparent') });
      const headers = new Headers(request.headers);
      headers.delete('host');
      headers.delete('content-length');
      const body = request.method === 'GET' || request.method === 'HEAD' ? undefined : await request.arrayBuffer();
      const upstream = await fetch(`${target}${url.pathname}${url.search}`, { method: request.method, headers, body });
      // fetch has already decoded any whole-body encoding, so the headers that
      // described the encoded body no longer describe what is forwarded.
      const answer = new Headers(upstream.headers);
      answer.delete('content-encoding');
      answer.delete('content-length');
      return new Response(upstream.body, { status: upstream.status, headers: answer });
    },
  });
  return { url: `http://127.0.0.1:${server.port}`, requests, stop: () => server.stop(true) };
}
