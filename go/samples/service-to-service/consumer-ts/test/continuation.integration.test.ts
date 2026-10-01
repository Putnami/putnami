/**
 * Go→TS continuation cell of the client matrix: the Go provider's catalog
 * change feed declares a cursor continuation, and this TypeScript consumer keeps
 * one generated stream open across an instance change. The two provider
 * instances are two real Go processes that share no memory: each seeds the same
 * change log from the same catalog, so a continuation placed by the cursor
 * alone lands correctly on either. The front in between is the routing
 * boundary, the one double in this file.
 */
import { ItemsClient, isGetItemsChangesNotFoundError } from '@example/go-items-client';
import type { Application } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { afterAll, beforeAll, describe, expect } from 'bun:test';
import { bindingConfig, CATALOG_API_KEY, type RunningProvider, startConsumer, startForeignProvider } from './harness';

const FEATURE = 'samples/go-first-party-client-matrix';
const REQUIREMENT = 'a-declared-continuation-survives-an-instance-change';
/** The negotiated SSE wire (clientcontract ADR 0013): the request header and the token both ends speak. */
const SSE_WIRE_HEADER = 'x-putnami-stream-wire';
const SSE_WIRE_V1 = 'putnami.sse.v1';
/** Revisions a fresh Go provider seeds: one per catalog item. */
const SEEDED_REVISIONS = 2;

/** What the front observed on one opening of the change feed. */
interface Opening {
  readonly query: Record<string, string>;
  readonly requested: string | null;
  acknowledged: string | null;
}

/**
 * Routes every connection to the current instance, the way a load balancer
 * does, and records what crossed it on the change feed: the query, the wire
 * the consumer asked for and the wire the provider acknowledged.
 */
function front(first: RunningProvider): { url: string; current: RunningProvider; openings: Opening[]; stop(): void } {
  const router = { url: '', current: first, openings: [] as Opening[], stop: () => {} };
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
      const upstream = await fetch(`${router.current.baseUrl}${url.pathname}${url.search}`, {
        method: request.method,
        headers,
        body,
        signal: request.signal,
      });
      if (seen) seen.acknowledged = upstream.headers.get(SSE_WIRE_HEADER);
      return new Response(upstream.body, { status: upstream.status, headers: upstream.headers });
    },
  });
  router.url = `http://127.0.0.1:${server.port}`;
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

describe('a TypeScript consumer keeps a Go change feed across an instance change', () => {
  let first: RunningProvider;
  let second: RunningProvider;
  let router: ReturnType<typeof front>;
  let consumer: Application;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    first = await startForeignProvider();
    second = await startForeignProvider();
    router = front(first);
    consumer = await startConsumer(
      bindingConfig(router.url, { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } }),
    );
  });

  afterAll(async () => {
    await consumer?.stop();
    router?.stop();
    await first?.stop();
    await second?.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  specTest(
    'continues on another provider process after the last change the consumer received',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-cursor-stream-continues-on-another-instance-after-the-last-delivered-change',
    },
    async () => {
      const items = consumer.context.get(ItemsClient);
      // Two seeded revisions, then three creations: the feed waits on the
      // first process, which is drained before any creation.
      const until = SEEDED_REVISIONS + 3;
      const received: { cursor: string; revision: bigint }[] = [];
      const stream = items.getItems_Changes({ query: { until: BigInt(until) } });
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onMessage((change) => received.push({ cursor: change.cursor, revision: change.revision }));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await eventually('the seeded revisions to reach the consumer', () => received.length === SEEDED_REVISIONS);
      expect(received.map((change) => change.revision)).toEqual([1n, 2n]);

      // The instance change: the front now routes to the second process and
      // the first one drains — its stream ends with no terminal. The two
      // processes share no memory: the second places the continuation by the
      // cursor alone, on the revisions it seeded from the same catalog.
      router.current = second;
      await first.stop();
      await eventually('the continuation to reach the second process', () => router.openings.length === 2);

      // Three creations through the front, on the second process; the last
      // one completes the feed.
      for (const name of ['Sprocket-0', 'Sprocket-1', 'Sprocket-2']) {
        // biome-ignore lint/performance/noAwaitInLoops: creations are ordered on purpose
        await items.postItems({ body: { name, price: 75n } });
      }
      expect(await ended).toBeUndefined();

      // Every revision once, in order, across both processes: the second
      // process sent nothing the consumer had received from the first.
      expect(received.map((change) => change.revision)).toEqual([1n, 2n, 3n, 4n, 5n]);
      expect(received.map((change) => change.cursor)).toEqual(['r1', 'r2', 'r3', 'r4', 'r5']);

      // One opening, one continuation after the last received change, and
      // nothing after the terminal. Both asked for the negotiated wire and
      // were acknowledged on the response head.
      expect(router.openings).toHaveLength(2);
      expect(router.openings[0]?.query).toEqual({ until: String(until) });
      expect(router.openings[1]?.query).toEqual({ until: String(until), cursor: 'r2' });
      for (const opening of router.openings) {
        expect(opening.requested).toBe(SSE_WIRE_V1);
        expect(opening.acknowledged).toBe(SSE_WIRE_V1);
      }
    },
  );

  specTest(
    'reads a position the Go provider never issued as its typed refusal',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-position-the-provider-never-issued-is-its-typed-refusal',
    },
    async () => {
      const before = router.openings.length;
      const stream = consumer.context.get(ItemsClient).getItems_Changes({ query: { cursor: 'r999999', until: 1n } });
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
