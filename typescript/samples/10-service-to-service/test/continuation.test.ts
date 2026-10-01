/**
 * TS→TS continuation cell of the client matrix: this provider's catalog change
 * feed declares a cursor continuation, and a generated TypeScript consumer
 * keeps one stream open across an instance change. The two provider instances
 * are two real instances of this application on their own ports, sharing the
 * process-wide change log the way two replicas share a durable store; the
 * front in between is the routing boundary, the one double in this file.
 */
import { type Application, application } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { afterAll, beforeAll, describe, expect } from 'bun:test';
import { ItemsClient, isGetItemsChangesNotFoundError, registerItemsClient } from '../clients/ts/src';
import { changes } from '../src/change-log';
import { app as createApp } from '../src/main';
import { items } from '../src/store';
import { CATALOG_API_KEY } from '../src/workload-identity';

const FEATURE = 'samples/ts-first-party-client-matrix';
const REQUIREMENT = 'a-declared-continuation-survives-an-instance-change';
/** The negotiated SSE wire (clientcontract ADR 0013): the request header and the token both ends speak. */
const SSE_WIRE_HEADER = 'x-putnami-stream-wire';
const SSE_WIRE_V1 = 'putnami.sse.v1';

/** One running provider instance of this sample. */
interface Instance {
  readonly url: string;
  readonly stop: () => Promise<void>;
}

/** What the front observed on one opening of the change feed. */
interface Opening {
  readonly query: Record<string, string>;
  readonly requested: string | null;
  acknowledged: string | null;
}

function reservePort(): number {
  const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
  const port = reservation.port;
  reservation.stop(true);
  return port;
}

async function startInstance(): Promise<Instance> {
  const port = reservePort();
  const app = createApp({ port });
  await app.start();
  return { url: `http://localhost:${port}`, stop: () => app.stop() };
}

/**
 * Routes every connection to the current instance, the way a load balancer
 * does, and records what crossed it on the change feed: the query, the wire
 * the consumer asked for and the wire the provider acknowledged.
 */
function front(): { url: string; current: Instance | undefined; openings: Opening[]; stop(): void } {
  const router = { url: '', current: undefined as Instance | undefined, openings: [] as Opening[], stop: () => {} };
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const seen: Opening | undefined =
        url.pathname === '/items/changes'
          ? {
              query: Object.fromEntries(url.searchParams),
              requested: request.headers.get(SSE_WIRE_HEADER),
              acknowledged: null,
            }
          : undefined;
      if (seen) router.openings.push(seen);
      const headers = new Headers(request.headers);
      headers.delete('host');
      const body = request.method === 'GET' || request.method === 'HEAD' ? undefined : await request.arrayBuffer();
      if (!router.current) throw new Error('no instance behind the front');
      const upstream = await fetch(`${router.current.url}${url.pathname}${url.search}`, {
        method: request.method,
        headers,
        body,
        signal: request.signal,
      });
      if (seen) seen.acknowledged = upstream.headers.get(SSE_WIRE_HEADER);
      return new Response(upstream.body, { status: upstream.status, headers: upstream.headers });
    },
  });
  router.url = `http://localhost:${server.port}`;
  router.stop = () => server.stop(true);
  return router;
}

async function eventually(what: string, condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // biome-ignore lint/performance/noAwaitInLoops: polling
    await Bun.sleep(2);
  }
}

describe('a TypeScript consumer keeps the change feed across an instance change', () => {
  let router: ReturnType<typeof front>;
  let first: Instance;
  let second: Instance;
  let consumer: Application;
  const originalConfig = process.env.CONFIG_DATA;
  /** The catalog is process-wide: what this file creates, it gives back. */
  const created: string[] = [];

  beforeAll(async () => {
    router = front();
    // Every application of this process — the two provider instances, which
    // are their own consumers too, and the consumer — binds to the front.
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url: router.url,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
          },
        },
      },
    });
    resetConfigLoader();
    first = await startInstance();
    second = await startInstance();
    router.current = first;
    consumer = application();
    registerItemsClient(consumer);
    await consumer.start();
  });

  afterAll(async () => {
    await consumer?.stop();
    await first?.stop();
    await second?.stop();
    router?.stop();
    for (const id of created) items.delete(id);
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  specTest(
    'continues on another instance after the last change the consumer received',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-cursor-stream-continues-on-another-instance-after-the-last-delivered-change',
    },
    async () => {
      const client = consumer.context.get(ItemsClient);
      // Every retained revision, then three creations before the drain and
      // one after it: the feed cannot complete on the first instance.
      const head = changes.head();
      const until = head + 4;
      const received: { cursor: string; revision: number }[] = [];
      const stream = client.getItemsChanges({ query: { until } });
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onMessage((change) => received.push({ cursor: change.cursor, revision: change.revision }));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await eventually('the retained revisions to reach the consumer', () => received.length === head);
      const expectedSoFar = Array.from({ length: head }, (_, index) => index + 1);
      expect(received.map((change) => change.revision)).toEqual(expectedSoFar);

      // Three creations through the front, on the first instance: the
      // consumer receives them, so the last one is the position the
      // continuation carries.
      for (const name of ['Sprocket-0', 'Sprocket-1', 'Sprocket-2']) {
        // biome-ignore lint/performance/noAwaitInLoops: creations are ordered on purpose
        const { item } = await client.postItems({ body: { name, price: 3.5, stock: 7 } });
        created.push(item.id);
      }
      await eventually('the three creations to reach the consumer', () => received.length === head + 3);

      // The instance change: the front now routes to the second instance and
      // the first one stops — its stream ends with no terminal.
      router.current = second;
      await first.stop();
      await eventually('the continuation to reach the second instance', () => router.openings.length === 2);

      // The last creation lands on the second instance and completes the feed.
      const { item: last } = await client.postItems({ body: { name: 'Sprocket-3', price: 3.5, stock: 7 } });
      created.push(last.id);
      expect(await ended).toBeUndefined();

      // Every revision once, in order, across both instances.
      const expected = Array.from({ length: until }, (_, index) => index + 1);
      expect(received.map((change) => change.revision)).toEqual(expected);
      expect(received.map((change) => change.cursor)).toEqual(expected.map((revision) => `r${revision}`));

      // One opening, one continuation after the last received change, and
      // nothing after the terminal. Both asked for the negotiated wire and
      // were acknowledged on the response head.
      expect(router.openings).toHaveLength(2);
      expect(router.openings[0]?.query).toEqual({ until: String(until) });
      expect(router.openings[1]?.query).toEqual({ until: String(until), cursor: `r${head + 3}` });
      for (const opening of router.openings) {
        expect(opening.requested).toBe(SSE_WIRE_V1);
        expect(opening.acknowledged).toBe(SSE_WIRE_V1);
      }
    },
  );

  specTest(
    'reads a position the provider never issued as its typed refusal',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-position-the-provider-never-issued-is-its-typed-refusal',
    },
    async () => {
      const before = router.openings.length;
      const stream = consumer.context.get(ItemsClient).getItemsChanges({ query: { cursor: 'r999999', until: 1 } });
      const terminal = await new Promise<Error>((resolve) => {
        stream.onMessage(() => resolve(new Error('a refused position delivered a change')));
        stream.onError(resolve);
        stream.onComplete(() => resolve(new Error('a refused position completed instead')));
      });
      expect(isGetItemsChangesNotFoundError(terminal)).toBe(true);
      expect(terminal).toMatchObject({ status: 404, code: 'not_found' });
      // A typed refusal is never reopened.
      expect(router.openings.length - before).toBe(1);
    },
  );
});
