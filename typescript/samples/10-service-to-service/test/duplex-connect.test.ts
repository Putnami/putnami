import type { Application } from '@putnami/application';
import { ConnectTransport, type DuplexStream } from '@putnami/client';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { afterAll, beforeAll, describe, expect } from 'bun:test';
import { app as createApp } from '../src/main';
import { CATALOG_API_KEY, CATALOG_KEY_HEADER } from '../src/workload-identity';

/**
 * The TypeScript Connect transport calls this provider's client stream and
 * bidirectional stream over HTTP/2, on the Connect route the gRPC plugin
 * serves. The generated clients keep carrying both shapes over WebSocket; this
 * file proves the Connect route serves them to the transport directly.
 */

const FEATURE = 'samples/ts-first-party-client-matrix';
const REQUIREMENT = 'duplex-streams-ride-connect-over-http2';
const ADJUST = '/catalog.items.v1.ItemsService/ListItemsByIdAdjust';
const NEGOTIATE = '/catalog.items.v1.ItemsService/ListItemsByIdNegotiate';

interface StockTotal {
  id: string;
  applied: number;
  total: number;
}

describe('duplex streams over Connect on HTTP/2', () => {
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

  function open(path: string): DuplexStream<{ delta: number }, StockTotal> {
    return new ConnectTransport(baseUrl, 'catalog.items.v1').duplex<{ delta: number }, StockTotal>(path, {
      params: { id: 'item-1' },
      headers: new Headers({ [CATALOG_KEY_HEADER]: CATALOG_API_KEY }),
    });
  }

  specTest(
    'the TypeScript transport calls the client and the bidirectional stream over HTTP/2',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-typescript-consumer-calls-a-client-and-a-bidirectional-rpc-over-http2',
    },
    async () => {
      const adjust = open(ADJUST);
      const adjusted = new Promise<StockTotal[]>((resolve, reject) => {
        const received: StockTotal[] = [];
        adjust.onMessage((total) => received.push(total));
        adjust.onError(reject);
        adjust.onComplete(() => resolve(received));
      });
      for (const delta of [5, -2, 7]) adjust.send({ delta });
      adjust.end();
      expect(await adjusted).toEqual([{ id: 'item-1', applied: 3, total: 10 }]);

      const negotiate = open(NEGOTIATE);
      const received: StockTotal[] = [];
      let answered: (() => void) | undefined;
      const completed = new Promise<void>((resolve, reject) => {
        negotiate.onMessage((total) => {
          received.push(total);
          answered?.();
        });
        negotiate.onError(reject);
        negotiate.onComplete(resolve);
      });
      for (const delta of [4, 6]) {
        // Each running total arrives before the next delta is sent.
        const next = new Promise<void>((resolve) => {
          answered = resolve;
        });
        negotiate.send({ delta });
        // biome-ignore lint/performance/noAwaitInLoops: the conversation is turn by turn on purpose
        await next;
      }
      expect(received).toEqual([
        { id: 'item-1', applied: 1, total: 4 },
        { id: 'item-1', applied: 2, total: 10 },
      ]);
      negotiate.end();
      await completed;
      expect(received.at(-1)).toEqual({ id: 'item-1', applied: 2, total: 10 });
    },
  );
});
