/**
 * Go→TS cell of the REST JSON client matrix: a real Go Putnami provider runs in
 * its own process, and this TypeScript consumer calls it through the clients
 * generated from that provider's own contract.
 *
 * Harness shape (D0.5): the provider is a subprocess this suite owns, started
 * from the binary `putnami build` already produced — never `go run` on sources.
 * It binds `PORT=0` and the suite learns the bound port from the reserved
 * `putnami.ready` log marker (protocols/runtime/ready_marker.go), so there is no
 * fixed port, no port file, no scan and no sleep. The provider is killed and
 * awaited in `afterAll`, so no child survives the task.
 */
import {
  AuditClient,
  BlobsClient,
  BodyFidelityClient,
  CredentialCheckClient,
  ItemsClient,
  isGetQuotesIdNotFoundError,
  QuotesClient,
  registerWhoamiClient,
  TenantCheckClient,
  WhoamiClient,
} from '@example/go-items-client';
import { type Application, api, application, endpoint, getCollector, http, telemetry } from '@putnami/application';
import { ClientCredentialError, CredentialRegistryClosedError } from '@putnami/client';
import { generateTypeScriptClient, readOpenApiSource } from '@putnami/client/generator';
import { resetConfigLoader } from '@putnami/runtime';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { app as createConsumer } from '../src/consumer';
import {
  ALL_CREDENTIALS,
  bindingConfig,
  CATALOG_API_KEY,
  CATALOG_KEY_HEADER,
  PROVIDER_PROJECT,
  type RunningProvider,
  startConsumer,
  startForeignProvider,
  USER_SUBJECT,
  USER_TOKEN,
  workspaceRoot,
} from './harness';

describe('a TypeScript consumer against the real Go provider', () => {
  let provider: RunningProvider;
  let consumer: Application;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    provider = await startForeignProvider();
    consumer = await startConsumer(
      // Only the api-key profile is bound: a TypeScript consumer cannot bind
      // the provider's service-token profile from configuration alone, so the
      // first declared alternative is unsatisfiable and the runtime falls
      // through to the second instead of dispatching a partially bound one.
      bindingConfig(provider.baseUrl, { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } }),
    );
  });

  afterAll(async () => {
    await consumer?.stop();
    await provider?.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  it('reads a typed 200 with the declared query string', async () => {
    const items = consumer.context.get(ItemsClient);

    const both = await items.getItems({ query: { search: 'et', limit: 10n } });
    expect(both.items?.map((item) => item.name)).toEqual(['Widget', 'Gadget']);

    const filtered = await items.getItems({ query: { search: 'Gad', limit: 10n } });
    expect(filtered.items?.map((item) => item.name)).toEqual(['Gadget']);

    const limited = await items.getItems({ query: { search: 'et', limit: 1n } });
    expect(limited.items?.length).toBe(1);
  });

  it('reads a typed 201 back from a typed request body, and a path parameter', async () => {
    const items = consumer.context.get(ItemsClient);
    const created = await items.postItems({ body: { name: 'Sprocket', price: 75n } });
    expect(created).toEqual({ id: '3', name: 'Sprocket', price: 75n });

    const fetched = await items.getItems_Id({ path: { id: '1' } });
    expect(fetched).toEqual({ id: '1', name: 'Widget', price: 100n });
  });

  it('lets the binding inject the declared api-key header', async () => {
    // No header is written here: the provider's `catalog-key` profile names it
    // and the binding supplies the value.
    const check = await consumer.context.get(CredentialCheckClient).getCredential_check();
    expect(check).toEqual({ profile: 'catalog-key', header: CATALOG_KEY_HEADER, presented: true });
  });

  it('carries an out-of-JS-precision integer and an explicit null intact', async () => {
    const fidelity = consumer.context.get(BodyFidelityClient);
    const sent = {
      enabled: false,
      count: 0n,
      label: '',
      signed: 9007199254740993n,
      unsigned: 18446744073709551615n,
      nullable: null,
    };
    const echoed = await fidelity.postBody_fidelity({ body: sent });
    // Every one of these survives only because the emitted client uses bigint
    // and a JSON codec that does not round-trip through Number.
    expect(echoed).toEqual(sent);
    expect(echoed.signed).toBe(9007199254740993n);
    expect(echoed.unsigned).toBe(18446744073709551615n);
    expect(echoed.nullable).toBeNull();
  });

  it('carries opaque JSON values to the Go provider and back without rounding an integer', async () => {
    const audit = consumer.context.get(AuditClient);
    // The provider holds `payload` and `note` as json.RawMessage, so an
    // integer past uint64 comes back as the same digits: the client reads it
    // as a bigint. `value` and `attributes` go through Go's encoding/json, so
    // they carry values a float64 holds exactly.
    const sent = {
      attributes: { actor: { id: 'u-1', roles: ['admin', 'owner'] }, gone: null },
      payload: { z: 1, a: [18446744073709551616n, 0.5, 'x y'], m: {} },
      value: { a: [1, 2.5, 'x', null, true], b: { c: false } },
      note: null,
    };
    const echoed = await audit.postAudit({ body: sent });
    expect(echoed).toEqual(sent);
    expect((echoed.payload as { a: unknown[] }).a[0]).toBe(18446744073709551616n);
    expect(echoed.note).toBeNull();

    // An absent optional member stays absent: it does not come back as null.
    const bare = await audit.postAudit({ body: { attributes: {}, payload: [], value: 'text' } });
    expect(Object.hasOwn(bare, 'note')).toBe(false);
    expect(bare).toEqual({ attributes: {}, payload: [], value: 'text' });
  });

  // The declared-error dimension of this cell is NOT asserted here, and not
  // skipped either: `putnami lint` removes a `.skip`, and a characterization
  // test that pinned the current behavior would pin a defect. The Go provider
  // declares its error schema as the whole wire envelope while the TypeScript
  // client validates a declared schema against the `details` member alone, so a
  // well-formed Go 404 is rejected as `client.response`. The scenario is
  // recorded `blocked` in ../../test-scenarios.json. The same dimension is
  // green on the three other cells of the matrix.

  // --- Raw octet cell: TypeScript consumer → Go provider ------------------

  it('carries every octet to the Go provider and back', async () => {
    const blobs = consumer.context.get(BlobsClient);
    const payloads: Record<string, Uint8Array> = {
      empty: new Uint8Array(0),
      'non-utf8': new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a]),
      'at-bound': new Uint8Array(4096).fill(0x7f),
    };
    for (const [name, payload] of Object.entries(payloads)) {
      const echoed = await blobs.postBlobs_Echo({ body: payload });
      expect(`${name}:${Buffer.from(echoed.body).toString('hex')}`).toBe(
        `${name}:${Buffer.from(payload).toString('hex')}`,
      );
      expect(echoed.contentType).toBe('application/octet-stream');
      expect(echoed.status).toBe(200);
    }
  });

  it('composes a path parameter and the injected credential with a raw octet response', async () => {
    const blobs = consumer.context.get(BlobsClient);
    const blob = await blobs.getBlobs_Id({ path: { id: '1' } });
    expect(Buffer.from(blob.body).toString('hex')).toBe('00fffe807f225c0a');
    // Zero octets are octets: the empty stored payload arrives empty, not absent.
    const empty = await blobs.getBlobs_Id({ path: { id: '2' } });
    expect(empty.body.byteLength).toBe(0);
  });

  it("reads the Go provider's declared refusals as typed errors", async () => {
    const blobs = consumer.context.get(BlobsClient);
    // Past the declared bound the emitted guard fires before a socket exists.
    await expect(blobs.postBlobs_Echo({ body: new Uint8Array(4097) })).rejects.toMatchObject({
      code: 'client.request',
    });
    // The declared 404 of a binary endpoint still arrives typed.
    await expect(blobs.getBlobs_Id({ path: { id: 'absent' } })).rejects.toMatchObject({
      status: 404,
      code: 'not_found',
    });
  });

  // --- SSE cell: TypeScript consumer → Go provider ------------------------

  it('streams the declared server stream through the generated client', async () => {
    const messages = await collectStream(
      consumer.context.get(ItemsClient).getItems_Id_Watch({ path: { id: '1' }, query: { follow: false } }),
    );
    expect(messages.length).toBe(1);
    expect(messages[0]).toEqual({ id: '1', name: 'Widget', price: 100n });
  });

  it('refuses a stream whose credential the provider rejects, before any message', async () => {
    const previous = process.env.CONFIG_DATA;
    const rejected = await startConsumer(
      bindingConfig(provider.baseUrl, { 'catalog-key': { source: 'static', value: 'not-the-catalog-key' } }),
    );
    try {
      const seen: unknown[] = [];
      const stream = rejected.context
        .get(ItemsClient)
        .getItems_Id_Watch({ path: { id: '1' }, query: { follow: false } });
      stream.onMessage((value) => seen.push(value));
      await expect(drainStream(stream)).rejects.toMatchObject({ status: 401 });
      // Admission failed, so nothing the provider could have streamed exists.
      expect(seen).toEqual([]);
    } finally {
      await rejected.stop();
      if (previous === undefined) delete process.env.CONFIG_DATA;
      else process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  it('refuses a websocket server stream and a duplex one whose credential the provider rejects', async () => {
    // Admission is decided per stream shape: the revision feed and the
    // conversation travel on the same socket as the client stream and are
    // refused the same way, before any frame.
    const previous = process.env.CONFIG_DATA;
    const rejected = await startConsumer(
      bindingConfig(provider.baseUrl, { 'catalog-key': { source: 'static', value: 'not-the-catalog-key' } }),
    );
    try {
      const history: unknown[] = [];
      const feed = rejected.context.get(ItemsClient).getItems_Id_History({ path: { id: '1' } });
      feed.onMessage((value) => history.push(value));
      await expect(drainStream(feed)).rejects.toMatchObject({ status: 401 });
      expect(history).toEqual([]);

      const totals: unknown[] = [];
      const conversation = rejected.context.get(ItemsClient).getItems_Id_Negotiate({ path: { id: '1' } });
      conversation.onMessage((value) => totals.push(value));
      conversation.send({ delta: 1n });
      conversation.end();
      await expect(drainStream(conversation)).rejects.toMatchObject({ status: 401 });
      expect(totals).toEqual([]);
    } finally {
      await rejected.stop();
      if (previous === undefined) delete process.env.CONFIG_DATA;
      else process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  it('ends a followed stream on consumer cancellation', async () => {
    const abort = new AbortController();
    const stream = consumer.context
      .get(ItemsClient)
      .getItems_Id_Watch({ path: { id: '1' }, query: { follow: true } }, { signal: abort.signal });
    let received = 0;
    const terminal = await new Promise<Error>((resolve) => {
      stream.onMessage(() => {
        received += 1;
        if (received === 2) abort.abort();
      });
      stream.onError(resolve);
      stream.onComplete(() => resolve(new Error('a cancelled stream completed instead')));
    });
    expect(received).toBeGreaterThanOrEqual(2);
    expect(terminal).toMatchObject({ code: 'client.canceled' });
  });

  // --- WebSocket cells: TypeScript consumer → Go provider -----------------

  it('follows the declared websocket-first order on a server stream', async () => {
    // The Go provider declares `websocket` before `sse` for this operation and
    // declares it resumable. This consumer says neither: it calls the generated
    // method, and the runtime opens the socket the declaration named.
    const messages = await collectStream(consumer.context.get(ItemsClient).getItems_Id_History({ path: { id: '1' } }));
    expect(messages).toEqual([
      { id: '1', revision: '1' },
      { id: '1', revision: '2' },
      { id: '1', revision: '3' },
    ]);
  });

  it('carries a client stream to its single declared result', async () => {
    const stream = consumer.context.get(ItemsClient).getItems_Id_Adjust({ path: { id: '1' } });
    const values = collectStream(stream);
    for (const delta of [5n, -2n, 7n]) stream.send({ delta });
    stream.end();
    // `end` is idempotent, and the single declared result arrives as the last
    // message before completion — one delivery channel, as in Go.
    stream.end();
    expect(await values).toEqual([{ id: '1', applied: 3n, total: 10n }]);
  });

  it('outlives the consumer half-close on a bidirectional stream', async () => {
    const stream = consumer.context.get(ItemsClient).getItems_Id_Negotiate({ path: { id: '1' } });
    const values = collectStream(stream);
    stream.send({ delta: 4n });
    stream.send({ delta: 6n });
    stream.end();
    expect(await values).toEqual([
      { id: '1', applied: 1n, total: 4n },
      { id: '1', applied: 2n, total: 10n },
      { id: '1', applied: 2n, total: 10n },
    ]);
  });

  it('refuses a duplex stream whose credential the provider rejects, before any message', async () => {
    // The Go provider refuses a first-party admission in band: a typed `error`
    // frame, then the close. The consumer must read that frame as the declared
    // status, not as a socket that closed without a terminal.
    const refusals = [
      [{ 'catalog-key': { source: 'static', value: 'not-the-catalog-key' } }, 'catalog.consumer', 401],
      [{ 'catalog-key': { source: 'static', value: CATALOG_API_KEY } }, 'an.unlisted.consumer', 403],
    ] as const;
    for (const [credentials, clientId, status] of refusals) {
      const previous = process.env.CONFIG_DATA;
      const configData = JSON.parse(bindingConfig(provider.baseUrl, credentials));
      configData.clients.clientId = clientId;
      const refused = await startConsumer(JSON.stringify(configData));
      try {
        const seen: unknown[] = [];
        const stream = refused.context.get(ItemsClient).getItems_Id_Adjust({ path: { id: '1' } });
        stream.onMessage((value) => seen.push(value));
        stream.send({ delta: 1n });
        stream.end();
        await expect(drainStream(stream)).rejects.toMatchObject({ status });
        // Admission failed, so nothing the provider could have answered exists.
        expect(seen).toEqual([]);
      } finally {
        await refused.stop();
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      }
    }
  });

  it('ends a bidirectional stream on consumer cancellation', async () => {
    const abort = new AbortController();
    const stream = consumer.context
      .get(ItemsClient)
      .getItems_Id_Negotiate({ path: { id: '1' } }, { signal: abort.signal });
    let received = 0;
    const terminal = new Promise<Error>((resolve) => {
      stream.onMessage(() => {
        received += 1;
        abort.abort();
      });
      stream.onError(resolve);
      stream.onComplete(() => resolve(new Error('a cancelled stream completed instead')));
    });
    stream.send({ delta: 1n });
    const error = await terminal;
    expect(received).toBeGreaterThanOrEqual(1);
    expect(error).toMatchObject({ code: 'client.canceled' });
  });

  it('refuses a burst past the declared queue depth instead of dropping it', async () => {
    // D0.7 on this runtime: what a client cannot buffer before admission is a
    // typed refusal, never a silently dropped message. The declared depth is 4.
    const stream = consumer.context.get(ItemsClient).getItems_Id_Adjust({ path: { id: '1' } });
    const seen: unknown[] = [];
    stream.onMessage((value) => seen.push(value));
    const terminal = drainStream(stream);
    try {
      for (let index = 0; index < 32; index += 1) stream.send({ delta: 1n });
    } catch {
      // The refusal may surface on the send that overflows or on the terminal.
    }
    await expect(terminal).rejects.toMatchObject({ code: 'client.request' });
    expect(seen).toEqual([]);
  });

  it('reads an undeclared Go failure as one, with its status and none of its prose', async () => {
    const failure = await consumer.context
      .get(ItemsClient)
      .getItems_Id({ path: { id: 'boom' } })
      .catch((error: unknown) => error);
    // 503 is not one of the errors this operation declares.
    expect(failure).toMatchObject({ status: 503 });
    expect(String((failure as Error).message)).not.toContain('shard');
    expect(String((failure as Error).message)).not.toContain('replica');
  });

  it('refuses a response that does not honor the declared success schema', async () => {
    // A real socket answering with a body that omits a declared required
    // property. The generated client must refuse it, not decode it loosely.
    const rogue = Bun.serve({ port: 0, fetch: () => Response.json({ name: 'Widget', price: 100 }) });
    const previous = process.env.CONFIG_DATA;
    const rogueConsumer = await startConsumer(bindingConfig(`http://127.0.0.1:${rogue.port}`, {}));
    try {
      const call = rogueConsumer.context.get(ItemsClient).getItems_Id({ path: { id: '1' } });
      await expect(call).rejects.toMatchObject({ code: 'client.response' });
    } finally {
      await rogueConsumer.stop();
      rogue.stop(true);
      if (previous !== undefined) process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  it('never calls anonymously when a declared credential is not configured', async () => {
    let reached = false;
    const observer = Bun.serve({
      port: 0,
      fetch: () => {
        reached = true;
        return Response.json({ profile: 'catalog-key', header: CATALOG_KEY_HEADER, presented: true });
      },
    });
    const previous = process.env.CONFIG_DATA;
    const anonymous = await startConsumer(bindingConfig(`http://127.0.0.1:${observer.port}`, {}));
    try {
      await expect(anonymous.context.get(CredentialCheckClient).getCredential_check()).rejects.toThrow();
      expect(reached).toBe(false);
    } finally {
      await anonymous.stop();
      observer.stop(true);
      if (previous !== undefined) process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  // --- Connect cells: TypeScript consumer → Go provider -------------------
  //
  // The Go provider declares Connect alone for the quotes operations, JSON
  // before protobuf. The consumer names neither: the committed client takes the
  // first declared encoding, and a recording proxy in front of the provider
  // shows the wire each call really took.

  it('reads a quote over Connect JSON, the first encoding the Go provider declares', async () => {
    await withRecordedConsumer(provider.baseUrl, createConsumer, async (consumerApp, wire) => {
      const quote = await consumerApp.context.get(QuotesClient).getQuotes_Id({ path: { id: '1' } });
      expect(quote).toEqual(GO_QUOTE);
      expectOneConnectCall(wire.requests, '/items.v1.ApiService/GetQuotes', 'application/json');
    });
  });

  it('streams quote ticks as Connect envelopes ended by one terminal', async () => {
    await withRecordedConsumer(provider.baseUrl, createConsumer, async (consumerApp, wire) => {
      const ticks = await collectStream(
        consumerApp.context.get(QuotesClient).getQuotes_Id_Ticks({ path: { id: '1' } }),
      );
      expect(ticks).toEqual(GO_TICKS);
      expectOneConnectCall(wire.requests, '/items.v1.ApiService/GetQuotesTicks', 'application/connect+json', false);
    });
  });

  it("reads the Go provider's declared Connect error as its generated type", async () => {
    const failure = await consumer.context
      .get(QuotesClient)
      .getQuotes_Id({ path: { id: 'absent' } })
      .catch((error: unknown) => error);
    expect(failure).toMatchObject({ service: 'items', method: 'getQuotes_Id', status: 404, code: 'not_found' });
    expect(isGetQuotesIdNotFoundError(failure)).toBe(true);
  });

  it('reads the same quote over Connect protobuf through a client emitted with protobuf first', async () => {
    // The Go bridge publishes JSON before protobuf, so the committed client
    // takes JSON for these two operations. A consumer that prefers protobuf
    // states the other order over the same published declaration, and the same
    // emitter renders that client.
    const emitted = await emitProtoFirstQuotesClient();
    try {
      const create = () => {
        const instance = application();
        emitted.register(instance);
        return instance;
      };
      await withRecordedConsumer(provider.baseUrl, create, async (consumerApp, wire) => {
        const quotes = consumerApp.context.get(emitted.QuotesClient) as unknown as EmittedQuotesClient;
        expect(await quotes.getQuotes_Id({ path: { id: '1' } })).toEqual(GO_QUOTE);
        expectOneConnectCall(wire.requests, '/items.v1.ApiService/GetQuotes', 'application/proto');
        wire.requests.length = 0;
        expect(await collectStream(quotes.getQuotes_Id_Ticks({ path: { id: '1' } }))).toEqual(GO_TICKS);
        expectOneConnectCall(wire.requests, '/items.v1.ApiService/GetQuotesTicks', 'application/connect+proto', false);
      });
    } finally {
      emitted.cleanup();
    }
  });

  it('reads the snapshot over Connect protobuf, the encoding its operation declares first', async () => {
    // GET /quotes/{id}/snapshot declares ConnectEncodings protobuf then JSON.
    // The committed client is the stock one: the declaration alone moves the
    // call onto protobuf, with no consumer branch and no re-emitted client.
    await withRecordedConsumer(provider.baseUrl, createConsumer, async (consumerApp, wire) => {
      const quote = await consumerApp.context.get(QuotesClient).getQuotes_Id_Snapshot({ path: { id: '1' } });
      expect(quote).toEqual(GO_QUOTE);
      expectOneConnectCall(wire.requests, '/items.v1.ApiService/GetQuotesSnapshot', 'application/proto');
    });
  });

  // --- Lifecycle: TypeScript consumer → Go provider -----------------------

  it('refuses every call through a retained client once its application stopped, and nothing reaches the provider', async () => {
    const wire = startWireRecorder(provider.baseUrl);
    const previous = process.env.CONFIG_DATA;
    process.env.CONFIG_DATA = bindingConfig(wire.url, { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } });
    resetConfigLoader();
    const stoppable = createConsumer();
    await stoppable.start();
    try {
      const quotes = stoppable.context.get(QuotesClient);
      const items = stoppable.context.get(ItemsClient);
      expect((await quotes.getQuotes_Id({ path: { id: '1' } })).id).toBe('1');
      await stoppable.stop();
      const before = wire.requests.length;
      // Credentialed Connect and anonymous REST alike: the registry the clients
      // were built from is closed, so no call leaves.
      await expect(quotes.getQuotes_Id({ path: { id: '1' } })).rejects.toBeInstanceOf(CredentialRegistryClosedError);
      await expect(items.getItems({ query: { search: 'et', limit: 10n } })).rejects.toBeInstanceOf(
        CredentialRegistryClosedError,
      );
      expect(wire.requests.length).toBe(before);
    } finally {
      wire.stop();
      if (previous === undefined) delete process.env.CONFIG_DATA;
      else process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  // --- Observability: TypeScript consumer → Go provider -------------------

  it('continues an incoming trace into both wires, measures each call once and exports no secret', async () => {
    // A real inbound request carrying W3C trace context reaches a consumer
    // endpoint, which calls the Go provider over Connect and over REST with the
    // request's own context. It writes no header and no span of its own.
    const consumerHttp = http({ port: 0 });
    const consumerApi = api({ autoScan: false });
    let consumerApp: Application | undefined;
    consumerApi.register(
      '/quote-and-items',
      endpoint()
        .returns({ quote: String, items: Number })
        .handle(async () => {
          if (!consumerApp) throw new Error('the consumer is not started');
          const quote = await consumerApp.context.get(QuotesClient).getQuotes_Id({ path: { id: '1' } });
          const list = await consumerApp.context.get(ItemsClient).getItems({ query: { search: 'et', limit: 10n } });
          return { quote: quote.id, items: list.items?.length ?? 0 };
        }),
      'GET',
    );
    const create = () => {
      consumerApp = createConsumer()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'go-items-consumer-tests' }))
        .use(consumerHttp)
        .use(consumerApi);
      return consumerApp;
    };
    await withRecordedConsumer(provider.baseUrl, create, async (_consumer, wire) => {
      const collector = getCollector();
      collector?.drainAll();
      collector?.drainSpans();
      const traceId = '33333333333333333333333333333333';
      const parentSpanId = '4444444444444444';
      const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/quote-and-items`, {
        headers: { traceparent: `00-${traceId}-${parentSpanId}-01` },
      });
      expect(await response.json()).toEqual({ quote: '1', items: 2 });

      const spans = collector?.drainSpans() ?? [];
      const server = spans.find((span) => span.kind === 2);
      expect(server).toMatchObject({ traceId, parentSpanId });
      for (const [operation, path] of [
        ['getQuotes_Id', '/items.v1.ApiService/GetQuotes'],
        ['getItems', '/items'],
      ] as const) {
        const call = spans.find((span) => span.name === operation);
        const attempt = spans.find((span) => span.name === `${operation} attempt`);
        expect(call).toMatchObject({ traceId, parentSpanId: server?.spanId });
        expect(attempt).toMatchObject({ traceId, parentSpanId: call?.spanId });
        // The provider receives the attempt span as its parent, on either wire.
        expect(wire.requests.find((request) => request.path === path)?.traceparent).toBe(
          `00-${traceId}-${attempt?.spanId}-01`,
        );
      }

      const buckets = collector?.drainAll() ?? [];
      const calls = buckets
        .flatMap((bucket) => bucket.counterSeries ?? [])
        .filter((series) => series.name === 'rpc.client.calls');
      const labels = { 'rpc.service': 'items', 'rpc.system': 'putnami', 'http.response.status_code': 200 };
      expect(calls).toHaveLength(2);
      expect(calls).toEqual(
        expect.arrayContaining([
          {
            name: 'rpc.client.calls',
            value: 1,
            attributes: { ...labels, 'network.protocol.name': 'connect', 'rpc.method': 'getQuotes_Id' },
          },
          {
            name: 'rpc.client.calls',
            value: 1,
            attributes: { ...labels, 'network.protocol.name': 'rest-json', 'rpc.method': 'getItems' },
          },
        ]),
      );
      const exported = JSON.stringify({ spans, buckets }, (_key, value) =>
        typeof value === 'bigint' ? value.toString() : value,
      );
      for (const forbidden of [CATALOG_API_KEY, 'search=', 'Widget', 'Gadget']) {
        expect(exported).not.toContain(forbidden);
      }
    });
  });

  it('measures each generated stream once at its terminal, under the transport that carried it', async () => {
    // The four stream shapes the Go provider declares, opened inside one traced
    // inbound request, plus one conversation the provider ends with its
    // declared error. A stream is one call however many messages it carries.
    // The consumer is bound to the provider itself: a WebSocket upgrade does not
    // cross the recording proxy the unary cells use.
    const consumerHttp = http({ port: 0 });
    const consumerApi = api({ autoScan: false });
    let consumerApp: Application | undefined;
    consumerApi.register(
      '/streams',
      endpoint()
        .returns({ watched: Number, revisions: Number, adjusted: Number, negotiated: Number, refused: String })
        .handle(async () => {
          if (!consumerApp) throw new Error('the consumer is not started');
          const items = consumerApp.context.get(ItemsClient);
          const watched = await collectStream(items.getItems_Id_Watch({ path: { id: '1' }, query: { follow: false } }));
          const revisions = await collectStream(items.getItems_Id_History({ path: { id: '1' } }));
          const adjust = items.getItems_Id_Adjust({ path: { id: '1' } });
          const adjusted = collectStream(adjust);
          adjust.send({ delta: 5n });
          adjust.end();
          const negotiate = items.getItems_Id_Negotiate({ path: { id: '1' } });
          const negotiated = collectStream(negotiate);
          negotiate.send({ delta: 4n });
          negotiate.end();
          const missing = items.getItems_Id_Adjust({ path: { id: 'missing' } });
          const refused = collectStream(missing).catch((error: { code?: string }) => error.code ?? '');
          missing.end();
          return {
            watched: watched.length,
            revisions: revisions.length,
            adjusted: (await adjusted).length,
            negotiated: (await negotiated).length,
            refused: String(await refused),
          };
        }),
      'GET',
    );
    const previous = process.env.CONFIG_DATA;
    process.env.CONFIG_DATA = bindingConfig(provider.baseUrl, {
      'catalog-key': { source: 'static', value: CATALOG_API_KEY },
    });
    resetConfigLoader();
    consumerApp = createConsumer()
      .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'go-items-consumer-tests' }))
      .use(consumerHttp)
      .use(consumerApi);
    await consumerApp.start();
    try {
      const collector = getCollector();
      collector?.drainAll();
      collector?.drainSpans();
      const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/streams`, {
        headers: { traceparent: `00-${TRACE_ID}-${PARENT_SPAN_ID}-01` },
      });
      expect(await response.json()).toEqual({
        watched: 1,
        revisions: 3,
        adjusted: 1,
        negotiated: 2,
        refused: 'not_found',
      });

      const spans = collector?.drainSpans() ?? [];
      const server = spans.find((span) => span.kind === 2);
      expect(server).toMatchObject({ traceId: TRACE_ID, parentSpanId: PARENT_SPAN_ID });
      for (const operation of ['getItems_Id_Watch', 'getItems_Id_History', 'getItems_Id_Negotiate']) {
        const calls = spans.filter((span) => span.name === operation);
        expect(calls).toHaveLength(1);
        expect(calls[0]).toMatchObject({ kind: 3, traceId: TRACE_ID, parentSpanId: server?.spanId, statusCode: 1 });
      }
      const adjusts = spans.filter((span) => span.name === 'getItems_Id_Adjust');
      expect(adjusts.map((span) => [span.parentSpanId, span.statusCode, span.statusMessage ?? '']).sort()).toEqual([
        [server?.spanId, 1, ''],
        [server?.spanId, 2, 'not_found'],
      ]);

      const buckets = collector?.drainAll() ?? [];
      const labels = { 'rpc.service': 'items', 'rpc.system': 'putnami' };
      const calls = buckets
        .flatMap((bucket) => bucket.counterSeries ?? [])
        .filter((series) => series.name === 'rpc.client.calls');
      expect(calls).toHaveLength(5);
      expect(calls).toEqual(
        expect.arrayContaining([
          {
            name: 'rpc.client.calls',
            value: 1,
            attributes: {
              ...labels,
              'network.protocol.name': 'sse',
              'rpc.method': 'getItems_Id_Watch',
              'http.response.status_code': 200,
            },
          },
          ...['getItems_Id_History', 'getItems_Id_Adjust', 'getItems_Id_Negotiate'].map((method) => ({
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
              'rpc.method': 'getItems_Id_Adjust',
              'http.response.status_code': 404,
              'error.type': 'not_found',
            },
          },
        ]),
      );
      const exported = JSON.stringify({ spans, buckets }, (_key, value) =>
        typeof value === 'bigint' ? value.toString() : value,
      );
      for (const forbidden of [CATALOG_API_KEY, 'follow=', 'Widget', 'missing']) {
        expect(exported).not.toContain(forbidden);
      }
      expectBoundedLabels(buckets);
    } finally {
      await consumerApp.stop();
      if (previous === undefined) delete process.env.CONFIG_DATA;
      else process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  it('traces a raw octet call and measures it once without exporting its octets', async () => {
    const consumerHttp = http({ port: 0 });
    const consumerApi = api({ autoScan: false });
    let consumerApp: Application | undefined;
    consumerApi.register(
      '/echo',
      endpoint()
        .returns({ bytes: Number, same: Boolean })
        .handle(async () => {
          if (!consumerApp) throw new Error('the consumer is not started');
          const echoed = await consumerApp.context.get(BlobsClient).postBlobs_Echo({ body: OCTET_MARKER });
          return {
            bytes: echoed.body.byteLength,
            same: Buffer.from(echoed.body).equals(Buffer.from(OCTET_MARKER)),
          };
        }),
      'GET',
    );
    const create = () => {
      consumerApp = createConsumer()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'go-items-consumer-tests' }))
        .use(consumerHttp)
        .use(consumerApi);
      return consumerApp;
    };
    await withRecordedConsumer(provider.baseUrl, create, async (_consumer, wire) => {
      const collector = getCollector();
      collector?.drainAll();
      collector?.drainSpans();
      const response = await fetch(`http://localhost:${consumerHttp.getServer()?.port}/echo`, {
        headers: { traceparent: `00-${TRACE_ID}-${PARENT_SPAN_ID}-01` },
      });
      expect(await response.json()).toEqual({ bytes: OCTET_MARKER.byteLength, same: true });

      const spans = collector?.drainSpans() ?? [];
      const server = spans.find((span) => span.kind === 2);
      const call = spans.find((span) => span.name === 'postBlobs_Echo');
      const attempt = spans.find((span) => span.name === 'postBlobs_Echo attempt');
      expect(call).toMatchObject({ traceId: TRACE_ID, parentSpanId: server?.spanId, statusCode: 1 });
      expect(attempt).toMatchObject({ traceId: TRACE_ID, parentSpanId: call?.spanId, statusCode: 1 });
      // The octets travel with the attempt span as their parent, like any call.
      expect(wire.requests.find((request) => request.path === '/blobs/echo')?.traceparent).toBe(
        `00-${TRACE_ID}-${attempt?.spanId}-01`,
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
            'rpc.service': 'items',
            'rpc.system': 'putnami',
            'rpc.method': 'postBlobs_Echo',
            'network.protocol.name': 'rest-json',
            'http.response.status_code': 200,
          },
        },
      ]);
      // Neither the octets nor any rendering of them reaches an export.
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
    });
  });

  // The identity families of the client matrix, across the language boundary:
  // two credentials one alternative requires together, and the caller's own
  // user identity taken from a real inbound request.
  describe('identity', () => {
    /** Run `body` against a consumer bound with exactly `credentials`. */
    async function withConsumer(credentials: Record<string, unknown>, body: (app: Application) => Promise<void>) {
      const previous = process.env.CONFIG_DATA;
      const bound = await startConsumer(bindingConfig(provider.baseUrl, credentials));
      try {
        await body(bound);
      } finally {
        await bound.stop();
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      }
    }

    it('reads the Go provider Connect refusal on a unary call and on a stream', async () => {
      await withConsumer({ 'catalog-key': { source: 'static', value: 'not-the-catalog-key' } }, async (bound) => {
        await expect(bound.context.get(QuotesClient).getQuotes_Id({ path: { id: '1' } })).rejects.toMatchObject({
          status: 401,
        });
        const seen: unknown[] = [];
        const stream = bound.context.get(QuotesClient).getQuotes_Id_Ticks({ path: { id: '1' } });
        stream.onMessage((value) => seen.push(value));
        await expect(drainStream(stream)).rejects.toMatchObject({ status: 401 });
        expect(seen).toEqual([]);
      });
    });

    it('carries both credentials of one alternative to the Go provider', async () => {
      await withConsumer(ALL_CREDENTIALS, async (bound) => {
        const check = await bound.context.get(TenantCheckClient).getTenant_check();
        expect(check).toEqual({ key: true, tenant: true });
      });
    });

    it('refuses a half-satisfied alternative before the request leaves the consumer', async () => {
      await withConsumer({ 'catalog-key': { source: 'static', value: CATALOG_API_KEY } }, async (bound) => {
        // The Go provider never sees this call: the refusal is the consumer's.
        await expect(bound.context.get(TenantCheckClient).getTenant_check()).rejects.toBeInstanceOf(
          ClientCredentialError,
        );
      });
    });

    it('lets the Go provider name the user this consumer forwarded', async () => {
      const previous = process.env.CONFIG_DATA;
      process.env.CONFIG_DATA = bindingConfig(provider.baseUrl, ALL_CREDENTIALS);
      resetConfigLoader();
      const server = http({ port: 0 });
      const bound = application().use(server);
      registerWhoamiClient(bound);
      // The route writes no header and passes no token: the runtime carries the
      // identity of the request in flight to the outbound call.
      server.get('/me', () => bound.context.get(WhoamiClient).getWhoami());
      await bound.start();
      try {
        const response = await fetch(`http://localhost:${server.getServer()?.port}/me`, {
          headers: { Authorization: `Bearer ${USER_TOKEN}` },
        });
        expect(response.status).toBe(200);
        expect(await response.json()).toEqual({ subject: USER_SUBJECT });

        // With no inbound identity there is nothing to forward, and the call
        // fails in the consumer instead of reaching the provider anonymously.
        const anonymous = await fetch(`http://localhost:${server.getServer()?.port}/me`);
        expect(anonymous.status).not.toBe(200);
      } finally {
        await bound.stop();
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      }
    });
  });
});

/** The Go provider's quote, member for member. */
const GO_QUOTE = {
  id: '1',
  units: 18446744073709551615n,
  offset: -7,
  fingerprint: new Uint8Array([0x00, 0xff, 0x80, 0x22]),
  tags: ['a', 'b'],
  labels: { k: 'v' },
};

const GO_TICKS = [
  { id: '1', sequence: 1n },
  { id: '1', sequence: 2n },
  { id: '1', sequence: 3n },
];

interface StreamHandle<T> {
  onMessage: (handler: (value: T) => void) => void;
  onError: (handler: (error: Error) => void) => void;
  onComplete: (handler: () => void) => void;
}

interface EmittedQuotesClient {
  getQuotes_Id(input: { path: { id: string } }): Promise<unknown>;
  getQuotes_Id_Ticks(input: { path: { id: string } }): StreamHandle<unknown>;
}

/** What one request carried to the provider. A credential value is never recorded. */
interface WireRequest {
  path: string;
  contentType: string | null;
  protocolVersion: string | null;
  timeoutMs: string | null;
  traceparent: string | null;
  keyPresented: boolean;
}

/**
 * A recording proxy in front of the provider. It forwards every request and
 * response unchanged, and logs the wire each request took.
 */
function startWireRecorder(target: string) {
  const requests: WireRequest[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      requests.push({
        path: url.pathname,
        contentType: request.headers.get('content-type'),
        protocolVersion: request.headers.get('connect-protocol-version'),
        timeoutMs: request.headers.get('connect-timeout-ms'),
        traceparent: request.headers.get('traceparent'),
        keyPresented: request.headers.get(CATALOG_KEY_HEADER) === CATALOG_API_KEY,
      });
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

function expectOneConnectCall(requests: WireRequest[], path: string, contentType: string, unary = true): void {
  expect(requests).toHaveLength(1);
  expect(requests[0]).toMatchObject({ path, contentType, protocolVersion: '1', keyPresented: true });
  // A unary call always states its remaining budget. A stream with no declared
  // duration has no deadline to state, and Connect makes the header optional;
  // when one is sent it must still be a positive budget.
  const timeout = requests[0]?.timeoutMs ?? null;
  if (unary || timeout !== null) expect(Number(timeout)).toBeGreaterThan(0);
}

/**
 * Start a consumer bound to a recording proxy in front of the real provider,
 * run the scenario, then restore the configuration the suite started with.
 */
async function withRecordedConsumer(
  providerUrl: string,
  create: () => Application,
  scenario: (consumerApp: Application, wire: { requests: WireRequest[] }) => Promise<void>,
): Promise<void> {
  const wire = startWireRecorder(providerUrl);
  const previous = process.env.CONFIG_DATA;
  process.env.CONFIG_DATA = bindingConfig(wire.url, { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } });
  resetConfigLoader();
  const consumerApp = create();
  await consumerApp.start();
  try {
    await scenario(consumerApp, wire);
  } finally {
    await consumerApp.stop();
    wire.stop();
    if (previous === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = previous;
    resetConfigLoader();
  }
}

/**
 * Emit the quotes client the Go provider's published contract implies for a
 * consumer that prefers protobuf: the committed document read by the strict
 * reader, with the protobuf transport moved first on the quotes operations
 * only, rendered by the same emitter the committed client came from. The
 * document is never round-tripped through `JSON.parse`, which would round its
 * 64-bit bounds.
 */
async function emitProtoFirstQuotesClient() {
  const ir = readOpenApiSource(
    readFileSync(join(workspaceRoot(), PROVIDER_PROJECT, 'schema', 'openapi.json'), 'utf8'),
    {
      mode: 'firstParty',
    },
  );
  for (const method of ir.services.flatMap((service) => service.methods)) {
    if (!method.path.startsWith('/quotes/') || !method.client) continue;
    method.client = {
      ...method.client,
      transports: [...method.client.transports].sort(
        (left, right) => Number(right.encoding === 'proto') - Number(left.encoding === 'proto'),
      ),
    };
  }
  const files = generateTypeScriptClient(ir, { packageName: '@example/go-quotes-proto-first' });
  const directory = mkdtempSync(join(tmpdir(), 'putnami-go-quotes-'));
  const runtime = Bun.resolveSync('@putnami/client', import.meta.dir);
  for (const file of files) {
    const destination = join(directory, file.path);
    mkdirSync(dirname(destination), { recursive: true });
    writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${runtime}'`));
  }
  const emitted = await import(join(directory, 'src', 'index.ts'));
  return {
    QuotesClient: emitted.QuotesClient as new (...args: never[]) => unknown,
    register: emitted.registerQuotesClient as (target: Application) => void,
    cleanup: () => rmSync(directory, { recursive: true, force: true }),
  };
}

/** Collect every message of a stream and resolve on its terminal. */
function collectStream<T>(stream: {
  onMessage: (handler: (value: T) => void) => void;
  onError: (handler: (error: Error) => void) => void;
  onComplete: (handler: () => void) => void;
}): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

/** Resolve on the terminal without collecting messages. */
function drainStream(stream: {
  onError: (handler: (error: Error) => void) => void;
  onComplete: (handler: () => void) => void;
}): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.onError(reject);
    stream.onComplete(() => resolve());
  });
}

/** The trace the stream and octet cells' inbound request carries, and the span that sent it. */
const TRACE_ID = '55555555555555555555555555555555';
const PARENT_SPAN_ID = '6666666666666666';

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
