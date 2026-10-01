import { type Application, application } from '@putnami/application';
import { CredentialRegistryClosedError } from '@putnami/client';
import { generateTypeScriptClient, readOpenApiSource } from '@putnami/client/generator';
import { resetConfigLoader } from '@putnami/runtime';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  type GetItemsIdNotFoundError,
  isGetItemsIdNotFoundError,
  isGetQuotesIdNotFoundError,
  ItemsClient,
  QuotesClient,
  registerQuotesClient,
} from '../clients/ts/src';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { app as createApp } from '../src/main';
import { items, SEEDED_ITEM_IDS } from '../src/store';
import { CATALOG_API_KEY, CATALOG_API_KEY_WITHOUT_SCOPE, CATALOG_KEY_HEADER } from '../src/workload-identity';

// The declared 404 narrows to a single exact type. Assigning it to a fully
// spelled-out literal type is the compile-time half of the proof: if generation
// widened the code, the service or the status, this stops compiling.
function throwExactGeneratedError(error: GetItemsIdNotFoundError): never {
  const exact: {
    readonly code: 'not_found';
    readonly service: 'catalog.items';
    readonly method: 'getItems_id';
    readonly status: 404;
    readonly details?: undefined;
  } = error;
  throw exact;
}

const LIST_INPUT = {
  query: { search: '', limit: 10 },
  headers: { 'x-catalog-tenant': 'sample-tenant' },
} as const;

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('service-to-service sample', () => {
  let app: Application;
  let baseUrl: string;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
    const port = reservation.port;
    reservation.stop(true);
    baseUrl = `http://localhost:${port}`;
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url: baseUrl,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
          },
        },
      },
    });
    resetConfigLoader();
    app = createApp({ port });
    await app.start();
  });

  afterAll(async () => {
    await app.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  it('should list items via the generated typed client', async () => {
    const result = await app.context.get(ItemsClient).getItems(LIST_INPUT);
    expect(result.items).toBeArray();
    // The store is shared by every test file of this process. Each file removes
    // what it creates, and counting the seeded items alone keeps this assertion
    // independent of file order even when one does not.
    expect(result.items.map((item) => item.id).filter((id) => SEEDED_ITEM_IDS.includes(id))).toEqual([
      'item-1',
      'item-2',
      'item-3',
    ]);
  });

  it('should carry the declared query string and request header', async () => {
    const client = app.context.get(ItemsClient);

    const filtered = await client.getItems({
      query: { search: 'Gadget', limit: 10 },
      headers: { 'x-catalog-tenant': 'tenant-a' },
    });
    expect(filtered.items.map((item) => item.name)).toEqual(['Gadget']);
    // The provider echoes the header it received, so this asserts the header
    // crossed the network — the consumer never wrote it on a request object.
    expect(filtered.tenant).toBe('tenant-a');

    const limited = await client.getItems({
      query: { search: '', limit: 1 },
      headers: { 'x-catalog-tenant': 'tenant-b' },
    });
    expect(limited.items.length).toBe(1);
    expect(limited.tenant).toBe('tenant-b');
  });

  it('should keep an absent optional property absent and a present one present', async () => {
    const client = app.context.get(ItemsClient);

    const stillSold = await client.getItems_id({ path: { id: 'item-1' } });
    expect('discontinuedAt' in stillSold.item).toBe(false);

    const discontinued = await client.getItems_id({ path: { id: 'item-2' } });
    expect(discontinued.item.discontinuedAt).toBe('2026-01-31');
  });

  it('should create an item with a typed body and read it back', async () => {
    const client = app.context.get(ItemsClient);
    const created = await client.postItems({ body: { name: 'Sprocket', price: 3.5, stock: 7 } });
    try {
      expect(created.item.name).toBe('Sprocket');

      const readBack = await client.getItems_id({ path: { id: created.item.id } });
      expect(readBack.item).toEqual(created.item);
    } finally {
      // The store is module state every test file of this process shares: an
      // item left behind changes what the next file lists.
      items.delete(created.item.id);
    }
  });

  specTest(
    'should narrow the declared 404 to its generated type with the stable code',
    {
      feature: FEATURE,
      requirement: 'a-result-is-what-the-contract-declared',
      check: 'a-declared-error-narrows-to-its-generated-type',
    },
    async () => {
      const client = app.context.get(ItemsClient);
      try {
        await client.getItems_id({ path: { id: 'missing' } });
        throw new Error('the missing item resolved');
      } catch (error: unknown) {
        if (!isGetItemsIdNotFoundError(error)) throw error;
        expect(error.code).toBe('not_found');
        expect(error.status).toBe(404);
        expect(error.service).toBe('catalog.items');
        expect(() => throwExactGeneratedError(error)).toThrow();
      }
    },
  );

  specTest(
    'should read an undeclared remote failure as one, with its status and none of its prose',
    {
      feature: FEATURE,
      requirement: 'a-result-is-what-the-contract-declared',
      check: 'an-undeclared-remote-failure-stays-untyped-and-carries-no-prose',
    },
    async () => {
      const failure = await app.context
        .get(ItemsClient)
        .getItems_id({ path: { id: 'boom' } })
        .catch((error: unknown) => error);
      // 503 is not one of the errors this operation declares, so nothing may
      // narrow it to a declared type.
      expect(isGetItemsIdNotFoundError(failure)).toBe(false);
      expect(failure).toMatchObject({ status: 503 });
      // The provider named a shard and a replica lag; neither is the consumer's
      // business and neither belongs in a client-side error.
      expect(String((failure as Error).message)).not.toContain('shard');
      expect(String((failure as Error).message)).not.toContain('replica');
    },
  );

  specTest(
    'should reject a response that does not honor the declared schema',
    {
      feature: FEATURE,
      requirement: 'a-result-is-what-the-contract-declared',
      check: 'a-response-that-violates-the-declared-schema-is-refused',
    },
    async () => {
      // A real socket, a real generated client, and a provider that answers with a
      // body missing a declared required property. The client must refuse it
      // rather than hand a lossy object to the caller.
      const rogue = Bun.serve({
        port: 0,
        fetch: () => Response.json({ item: { name: 'Widget', price: 1, stock: 1 } }),
      });
      const previous = process.env.CONFIG_DATA;
      process.env.CONFIG_DATA = JSON.stringify({
        clients: {
          clientId: 'service-to-service-sample',
          services: { 'catalog.items': { url: `http://localhost:${rogue.port}`, allowInsecure: true } },
        },
      });
      resetConfigLoader();
      const rogueApp = createApp({ port: 0 });
      await rogueApp.start();
      try {
        const call = rogueApp.context.get(ItemsClient).getItems_id({ path: { id: 'item-1' } });
        await expect(call).rejects.toMatchObject({ code: 'client.response' });
      } finally {
        await rogueApp.stop();
        rogue.stop(true);
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      }
    },
  );

  it('should resolve the typed client in the consumer endpoint without manual HTTP wiring', async () => {
    const result = (await fetch(`${baseUrl}/proxy`).then((response) => response.json())) as {
      message: string;
      tenant: string;
      items: unknown[];
    };
    expect(result.message).toBe('Fetched items through the generated service binding');
    expect(result.tenant).toBe('sample-tenant');
    expect(result.items.length).toBeGreaterThanOrEqual(3);
  });

  it('should expose OpenAPI spec', async () => {
    const res = await fetch(`${baseUrl}/openapi.json`);
    expect(res.status).toBe(200);
  });

  // --- SSE cell: TypeScript consumer → TypeScript provider ----------------

  it('should stream the declared server stream through the generated client', async () => {
    const messages = await collectStream(
      app.context.get(ItemsClient).getItems_idWatch({ path: { id: 'item-1' }, query: { follow: false } }),
    );
    expect(messages.length).toBe(1);
    expect(messages[0]?.id).toBe('item-1');
    expect(messages[0]?.name).toBe('Widget');
  });

  // --- WebSocket server-stream cell: the declared order picks the socket ---

  it('should follow the declared websocket-first order on a server stream', async () => {
    // This provider declares `websocket` before `sse` for the revision feed and
    // declares it resumable. The consumer says neither: it calls the generated
    // method, and the runtime opens the socket the declaration named.
    const messages = await collectStream(app.context.get(ItemsClient).getItems_idHistory({ path: { id: 'item-1' } }));
    expect(messages).toEqual([
      { id: 'item-1', revision: '1' },
      { id: 'item-1', revision: '2' },
      { id: 'item-1', revision: '3' },
    ]);
  });

  it('should narrow the declared stream 404 to its generated type', async () => {
    const stream = app.context.get(ItemsClient).getItems_idWatch({ path: { id: 'missing' }, query: { follow: false } });
    await expect(collectStream(stream)).rejects.toMatchObject({ code: 'not_found', status: 404 });
  });

  it('should refuse a stream whose credential the provider rejects, before any message', async () => {
    for (const [key, status] of [
      [undefined, 401],
      [CATALOG_API_KEY_WITHOUT_SCOPE, 403],
    ] as const) {
      const consumer = await consumerBoundWith(key);
      try {
        const seen: unknown[] = [];
        const stream = consumer.client.getItems_idWatch({ path: { id: 'item-1' }, query: { follow: false } });
        stream.onMessage((value) => seen.push(value));
        await expect(drainStream(stream)).rejects.toMatchObject({ status });
        // Admission failed, so nothing the provider could have streamed exists.
        expect(seen).toEqual([]);
      } finally {
        await consumer.stop();
      }
    }
  });

  it('should end a followed stream on consumer cancellation', async () => {
    // The consumer cancels the way the generated method offers: an AbortSignal
    // it already owns, never a transport handle.
    const abort = new AbortController();
    const stream = app.context
      .get(ItemsClient)
      .getItems_idWatch({ path: { id: 'item-1' }, query: { follow: true } }, { signal: abort.signal });
    let received = 0;
    const terminal = new Promise<Error>((resolve) => {
      stream.onMessage(() => {
        received += 1;
        if (received === 2) abort.abort();
      });
      stream.onError(resolve);
      stream.onComplete(() => resolve(new Error('a cancelled stream completed instead')));
    });
    const error = await terminal;
    expect(received).toBeGreaterThanOrEqual(2);
    expect(error).toMatchObject({ code: 'client.canceled' });
  });

  // --- WebSocket cells: TypeScript consumer → TypeScript provider ---------

  it('should carry a client stream to its single declared result', async () => {
    const stream = app.context.get(ItemsClient).getItems_idAdjust({ path: { id: 'item-1' } });
    const values = collectStreamValues(stream);
    for (const delta of [5, -2, 7]) stream.send({ delta });
    stream.end();
    // `end` is idempotent: a consumer that half-closes on its own exit path as
    // well as at the end of its loop must not be punished for it.
    stream.end();
    const messages = await values;
    // The single declared result arrives as the last message before completion,
    // exactly as it does in Go. There is no second delivery channel.
    expect(messages).toEqual([{ id: 'item-1', applied: 3, total: 10 }]);
  });

  it('should outlive the consumer half-close on a bidirectional stream', async () => {
    const stream = app.context.get(ItemsClient).getItems_idNegotiate({ path: { id: 'item-1' } });
    const values = collectStreamValues(stream);
    stream.send({ delta: 4 });
    stream.send({ delta: 6 });
    stream.end();
    const messages = await values;
    // Two running totals, then the terminal value the provider returned after
    // the half-close.
    expect(messages).toEqual([
      { id: 'item-1', applied: 1, total: 4 },
      { id: 'item-1', applied: 2, total: 10 },
      { id: 'item-1', applied: 2, total: 10 },
    ]);
  });

  it('should narrow a declared duplex terminal error to its generated type', async () => {
    for (const stream of [
      app.context.get(ItemsClient).getItems_idAdjust({ path: { id: 'missing' } }),
      app.context.get(ItemsClient).getItems_idNegotiate({ path: { id: 'missing' } }),
    ]) {
      const values = collectStreamValues(stream);
      stream.end();
      await expect(values).rejects.toMatchObject({ code: 'not_found', status: 404 });
    }
  });

  it('should refuse a duplex stream whose credential the provider rejects, before any message', async () => {
    for (const [key, status] of [
      [undefined, 401],
      [CATALOG_API_KEY_WITHOUT_SCOPE, 403],
    ] as const) {
      const consumer = await consumerBoundWith(key);
      try {
        const seen: unknown[] = [];
        const stream = consumer.client.getItems_idAdjust({ path: { id: 'item-1' } });
        stream.onMessage((value) => seen.push(value));
        stream.send({ delta: 1 });
        stream.end();
        await expect(drainStream(stream)).rejects.toMatchObject({ status });
        // Admission failed, so nothing the provider could have answered exists.
        expect(seen).toEqual([]);
      } finally {
        await consumer.stop();
      }
    }
  });

  specTest(
    'should refuse a websocket server stream whose credential the provider rejects',
    {
      feature: FEATURE,
      requirement: 'identity-is-declared-and-never-downgraded',
      check: 'a-refused-stream-credential-arrives-before-any-message',
    },
    async () => {
      // Admission is decided per stream shape, not per wire: the revision feed
      // travels on the same socket as the conversations and is refused the same
      // way, before its first message.
      for (const [key, status] of [
        [undefined, 401],
        [CATALOG_API_KEY_WITHOUT_SCOPE, 403],
      ] as const) {
        const consumer = await consumerBoundWith(key);
        try {
          const seen: unknown[] = [];
          const stream = consumer.client.getItems_idHistory({ path: { id: 'item-1' } });
          stream.onMessage((value) => seen.push(value));
          await expect(drainStream(stream)).rejects.toMatchObject({ status });
          expect(seen).toEqual([]);
        } finally {
          await consumer.stop();
        }
      }
    },
  );

  it('should refuse a bidirectional stream whose credential the provider rejects', async () => {
    for (const [key, status] of [
      [undefined, 401],
      [CATALOG_API_KEY_WITHOUT_SCOPE, 403],
    ] as const) {
      const consumer = await consumerBoundWith(key);
      try {
        const seen: unknown[] = [];
        const stream = consumer.client.getItems_idNegotiate({ path: { id: 'item-1' } });
        stream.onMessage((value) => seen.push(value));
        stream.send({ delta: 1 });
        stream.end();
        await expect(drainStream(stream)).rejects.toMatchObject({ status });
        expect(seen).toEqual([]);
      } finally {
        await consumer.stop();
      }
    }
  });

  it('should end a bidirectional stream on consumer cancellation', async () => {
    const abort = new AbortController();
    const stream = app.context
      .get(ItemsClient)
      .getItems_idNegotiate({ path: { id: 'item-1' } }, { signal: abort.signal });
    let received = 0;
    const terminal = new Promise<Error>((resolve) => {
      stream.onMessage(() => {
        received += 1;
        abort.abort();
      });
      stream.onError(resolve);
      stream.onComplete(() => resolve(new Error('a cancelled stream completed instead')));
    });
    stream.send({ delta: 1 });
    const error = await terminal;
    expect(received).toBeGreaterThanOrEqual(1);
    expect(error).toMatchObject({ code: 'client.canceled' });
  });

  it('should refuse a burst past the declared queue depth instead of dropping it', async () => {
    // D0.7 on this runtime: what a client cannot buffer before admission is a
    // typed refusal, never a silently dropped message. The declared depth is 4,
    // and the Go client slows the socket instead — the two mechanisms differ,
    // the guarantee does not: no message is lost without the caller learning it.
    const stream = app.context.get(ItemsClient).getItems_idAdjust({ path: { id: 'item-1' } });
    const seen: unknown[] = [];
    stream.onMessage((value) => seen.push(value));
    const terminal = drainStream(stream);
    try {
      for (let index = 0; index < 32; index += 1) stream.send({ delta: 1 });
    } catch {
      // The refusal may surface on the send that overflows or on the terminal;
      // the assertion below is on the terminal, which is what a caller reads.
    }
    await expect(terminal).rejects.toMatchObject({ code: 'client.request' });
    expect(seen).toEqual([]);
  });

  // --- Connect cells: TypeScript consumer → TypeScript provider -----------
  //
  // The provider declares Connect alone for the quotes routes, protobuf before
  // JSON. A consumer that says neither dispatches the first declared encoding,
  // and a recording proxy in front of the provider shows the wire it took.

  it('should carry a quote and its ticks over Connect protobuf, the first declared encoding', async () => {
    const wire = startWireRecorder(baseUrl);
    const consumer = await quotesConsumerBoundTo(wire.url);
    try {
      const quote = await consumer.client.getQuotes_id({ path: { id: '1' } });
      // 2^53 - 1 is the widest integer a TypeScript provider holds exactly; the
      // negative 32-bit value exercises the sign extension the wire requires.
      expect(quote).toEqual({ id: '1', units: 9007199254740991n, offset: -7, tags: ['a', 'b'] });
      expectOneConnectCall(wire.requests, '/catalog.items.v1.QuotesService/GetQuotesById', 'application/proto');

      wire.requests.length = 0;
      const ticks = await collectStream(consumer.client.getQuotes_idTicks({ path: { id: '1' } }));
      expect(ticks).toEqual([
        { id: '1', sequence: 1n },
        { id: '1', sequence: 2n },
        { id: '1', sequence: 3n },
      ]);
      expectOneConnectCall(
        wire.requests,
        '/catalog.items.v1.QuotesService/ListQuotesByIdTicks',
        'application/connect+proto',
        false,
      );
    } finally {
      await consumer.stop();
      wire.stop();
    }
  });

  it('should carry the same quote over Connect JSON through a client emitted with JSON first', async () => {
    // The provider declares both Connect encodings; a stock client dispatches
    // the first. This one is emitted from the same committed document with the
    // JSON transport moved ahead, so the second declared encoding is exercised
    // against the same running provider rather than assumed.
    const emitted = await emitJsonFirstQuotesClient();
    const wire = startWireRecorder(baseUrl);
    const previous = process.env.CONFIG_DATA;
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url: wire.url,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
          },
        },
      },
    });
    resetConfigLoader();
    const consumerApp = application();
    emitted.register(consumerApp);
    await consumerApp.start();
    try {
      const client = consumerApp.context.get(emitted.QuotesClient) as {
        getQuotes_id(input: { path: { id: string } }): Promise<unknown>;
        getQuotes_idTicks(input: { path: { id: string } }): StreamHandle<unknown>;
      };
      const quote = await client.getQuotes_id({ path: { id: '1' } });
      expect(quote).toEqual({ id: '1', units: 9007199254740991n, offset: -7, tags: ['a', 'b'] });
      expectOneConnectCall(wire.requests, '/catalog.items.v1.QuotesService/GetQuotesById', 'application/json');

      wire.requests.length = 0;
      const ticks = await collectStream(client.getQuotes_idTicks({ path: { id: '1' } }));
      expect(ticks).toEqual([
        { id: '1', sequence: 1n },
        { id: '1', sequence: 2n },
        { id: '1', sequence: 3n },
      ]);
      expectOneConnectCall(
        wire.requests,
        '/catalog.items.v1.QuotesService/ListQuotesByIdTicks',
        'application/connect+json',
        false,
      );
    } finally {
      await consumerApp.stop();
      wire.stop();
      emitted.cleanup();
      if (previous === undefined) delete process.env.CONFIG_DATA;
      else process.env.CONFIG_DATA = previous;
      resetConfigLoader();
    }
  });

  it('should carry the snapshot over Connect JSON, the encoding its operation declares first', async () => {
    // GET /quotes/[id]/snapshot declares `connectEncodings: ['json', 'proto']`.
    // The committed client is the stock one: the declaration alone moves the
    // call onto JSON, with no consumer branch and no re-emitted client.
    const wire = startWireRecorder(baseUrl);
    const consumer = await quotesConsumerBoundTo(wire.url);
    try {
      const quote = await consumer.client.getQuotes_idSnapshot({ path: { id: '1' } });
      expect(quote).toEqual({ id: '1', units: 9007199254740991n, offset: -7, tags: ['a', 'b'] });
      expectOneConnectCall(wire.requests, '/catalog.items.v1.QuotesService/ListQuotesByIdSnapshot', 'application/json');
    } finally {
      await consumer.stop();
      wire.stop();
    }
  });

  it('should refuse a Connect call whose credential the provider rejects', async () => {
    for (const [key, status] of [
      [undefined, 401],
      [CATALOG_API_KEY_WITHOUT_SCOPE, 403],
    ] as const) {
      const consumer = await quotesConsumerBoundTo(baseUrl, key ?? 'not-the-catalog-key');
      try {
        // The refusal is the provider's, and it arrives as the Connect error the
        // wire carries — not as a decoding failure.
        await expect(consumer.client.getQuotes_id({ path: { id: '1' } })).rejects.toMatchObject({ status });
        // A Connect stream states its refusal in the terminal the protocol
        // reserves for it, so the consumer reads the same status there.
        const seen: unknown[] = [];
        const stream = consumer.client.getQuotes_idTicks({ path: { id: '1' } });
        stream.onMessage((value) => seen.push(value));
        await expect(drainStream(stream)).rejects.toMatchObject({ status });
        expect(seen).toEqual([]);
      } finally {
        await consumer.stop();
      }
    }
  });

  it('should narrow a declared Connect error to its generated type with its declared details', async () => {
    const failure = await app.context
      .get(QuotesClient)
      .getQuotes_id({ path: { id: 'absent' } })
      .catch((error: unknown) => error);
    expect(failure).toMatchObject({
      service: 'catalog.items',
      method: 'getQuotes_id',
      status: 404,
      code: 'not_found',
      details: { resource: 'quote', id: 'absent' },
    });
    expect(isGetQuotesIdNotFoundError(failure)).toBe(true);
  });

  specTest(
    'should refuse every call through a retained client once its application stopped',
    {
      feature: FEATURE,
      requirement: 'a-call-is-observable-and-ends-with-its-application',
      check: 'a-retained-client-refuses-every-call-after-its-application-stopped',
    },
    async () => {
      const wire = startWireRecorder(baseUrl);
      const consumer = await quotesConsumerBoundTo(wire.url);
      let stopped = false;
      try {
        expect((await consumer.client.getQuotes_id({ path: { id: '1' } })).id).toBe('1');
        await consumer.stop();
        stopped = true;
        const before = wire.requests.length;
        await expect(consumer.client.getQuotes_id({ path: { id: '1' } })).rejects.toBeInstanceOf(
          CredentialRegistryClosedError,
        );
        // Nothing left the stopped consumer.
        expect(wire.requests.length).toBe(before);
      } finally {
        if (!stopped) await consumer.stop();
        wire.stop();
      }
    },
  );

  /** A consumer that registers the generated quotes client alone, bound to `url`. */
  async function quotesConsumerBoundTo(
    url: string,
    key: string = CATALOG_API_KEY,
  ): Promise<{ client: QuotesClient; stop: () => Promise<void> }> {
    const previous = process.env.CONFIG_DATA;
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: key } },
          },
        },
      },
    });
    resetConfigLoader();
    const consumerApp = application();
    registerQuotesClient(consumerApp);
    await consumerApp.start();
    return {
      client: consumerApp.context.get(QuotesClient),
      stop: async () => {
        await consumerApp.stop();
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      },
    };
  }

  /** Start a second consumer application bound to the same provider with `token`. */
  async function consumerBoundWith(
    key: string | undefined,
  ): Promise<{ client: ItemsClient; stop: () => Promise<void> }> {
    const previous = process.env.CONFIG_DATA;
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url: baseUrl,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: key ?? 'not-the-catalog-key' } },
          },
        },
      },
    });
    resetConfigLoader();
    const consumerApp = createApp({ port: 0 });
    await consumerApp.start();
    return {
      client: consumerApp.context.get(ItemsClient),
      stop: async () => {
        await consumerApp.stop();
        if (previous === undefined) delete process.env.CONFIG_DATA;
        else process.env.CONFIG_DATA = previous;
        resetConfigLoader();
      },
    };
  }
});

/** Collect every message of a stream and resolve on its terminal. */
function collectStream<T>(stream: {
  onMessage: (h: (value: T) => void) => void;
  onError: (h: (error: Error) => void) => void;
  onComplete: (h: () => void) => void;
}): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

/** Collect every message of a duplex stream and resolve on its terminal. */
function collectStreamValues<TIn, TOut>(stream: {
  onMessage: (h: (value: TOut) => void) => void;
  onError: (h: (error: Error) => void) => void;
  onComplete: (h: () => void) => void;
  send: (value: TIn) => void;
  end: () => void;
}): Promise<TOut[]> {
  return collectStream(stream);
}

/** What one request carried to the provider. A credential value is never recorded. */
interface WireRequest {
  path: string;
  contentType: string | null;
  protocolVersion: string | null;
  timeoutMs: string | null;
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

/** Resolve on the terminal without collecting messages. */
function drainStream(stream: {
  onError: (h: (error: Error) => void) => void;
  onComplete: (h: () => void) => void;
}): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.onError(reject);
    stream.onComplete(() => resolve());
  });
}

interface StreamHandle<T> {
  onMessage: (handler: (value: T) => void) => void;
  onError: (handler: (error: Error) => void) => void;
  onComplete: (handler: () => void) => void;
}

/**
 * Emit the quotes client this provider's published contract implies for a
 * consumer that prefers JSON: the committed document read by the strict
 * reader, with the JSON transport moved first on the quotes operations only,
 * rendered by the emitter the committed client came from. The document is
 * never round-tripped through `JSON.parse`, which would round its 64-bit
 * bounds.
 */
async function emitJsonFirstQuotesClient() {
  const ir = readOpenApiSource(readFileSync(join(import.meta.dir, '..', 'schema', 'openapi.json'), 'utf8'), {
    mode: 'firstParty',
  });
  for (const method of ir.services.flatMap((service) => service.methods)) {
    if (!method.path.startsWith('/quotes/') || !method.client) continue;
    method.client = {
      ...method.client,
      transports: [...method.client.transports].sort(
        (left, right) => Number(right.encoding === 'json') - Number(left.encoding === 'json'),
      ),
    };
  }
  const files = generateTypeScriptClient(ir, { packageName: '@example/items-quotes-json-first' });
  const directory = mkdtempSync(join(tmpdir(), 'putnami-ts-quotes-'));
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
