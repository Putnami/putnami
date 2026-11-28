import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import { heapStats } from 'bun:jsc';
import { AsyncLocalStorage } from 'node:async_hooks';
import { createEnvelope } from '@putnami/application';
import { runInContext } from '@putnami/runtime';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { retryInterceptor } from '../../src/runtime/retry';
import { serviceDeadlineInterceptor } from '../../src/runtime/service-resilience';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

// Bun keeps a signal AbortSignal.any builds, listeners included, alive while
// one of its sources is alive and not aborted. A call made while a request is
// served, and that settles, aborts neither the served request's signal nor
// its own timeout: a listener on the call's signal that reaches the served
// request would keep the request alive. Each scene serves requests that make
// one call, leaves such a listener on the call's signal, and counts the
// requests a full collection leaves behind.

const SERVED = 50;
const encoder = new TextEncoder();

async function retainedRequests(): Promise<number> {
  for (let i = 0; i < 5; i++) {
    Bun.gc(true);
    // biome-ignore lint/performance/noAwaitInLoops: each collection lets finalizers run before the next
    await Bun.sleep(10);
  }
  return heapStats().objectTypeCounts['Request'] ?? 0;
}

/** Serves `SERVED` requests, each answered once `call` settles, and returns how many stay alive. */
async function retainedAfterServing(call: (served: Request) => Promise<unknown>): Promise<number> {
  const front = Bun.serve({
    port: 0,
    async fetch(served) {
      await call(served);
      return new Response('ok');
    },
  });
  try {
    const before = await retainedRequests();
    for (let i = 0; i < SERVED; i++) {
      // biome-ignore lint/performance/noAwaitInLoops: requests are served one after the other
      const response = await fetch(`http://localhost:${front.port}/`);
      expect(await response.text()).toBe('ok');
    }
    await Bun.sleep(20);
    return (await retainedRequests()) - before;
  } finally {
    front.stop(true);
  }
}

/** A `next` that answers at once and leaves a listener reaching the served request on the call's signal. */
function answeringNext(served: Request) {
  return (request: ClientRequest): Promise<ClientResponse> => {
    request.signal?.addEventListener('abort', () => void served.url);
    return Promise.resolve({ data: {}, status: 200, headers: new Headers() });
  };
}

describe('request-path client signals', () => {
  it('the service deadline retains no served request once its call settles', async () => {
    const interceptor = serviceDeadlineInterceptor();
    const retained = await retainedAfterServing(
      async (served) =>
        await runInContext({ signal: served.signal }, () =>
          interceptor(
            {
              method: 'GET',
              path: '/items',
              headers: new Headers(),
              clientOperation: { stream: 'unary', idempotency: { kind: 'idempotent' }, errors: [] },
            } as unknown as ClientRequest,
            answeringNext(served),
          ),
        ),
    );
    expect(retained).toBeLessThan(SERVED / 5);
  });

  it('a retry attempt retains no served request once it settles', async () => {
    const interceptor = retryInterceptor({ maxRetries: 0 }, { timeoutMs: 60_000 });
    const retained = await retainedAfterServing((served) =>
      interceptor(
        { method: 'GET', path: '/items', headers: new Headers(), signal: served.signal },
        answeringNext(served),
      ),
    );
    expect(retained).toBeLessThan(SERVED / 5);
  });

  describe('a Connect server stream', () => {
    let backend: ReturnType<typeof Bun.serve>;
    const realFetch = globalThis.fetch;
    const servedRequest = new AsyncLocalStorage<Request>();

    beforeAll(() => {
      backend = Bun.serve({
        port: 0,
        fetch: () =>
          new Response(new Blob([createEnvelope(0x02, encoder.encode('{}')) as Uint8Array<ArrayBuffer>]), {
            headers: { 'Content-Type': 'application/connect+json', 'Connect-Protocol-Version': '1' },
          }),
      });
    });

    afterEach(() => {
      globalThis.fetch = realFetch;
    });

    afterAll(() => {
      backend.stop(true);
    });

    it('retains no served request once the stream ends', async () => {
      // The fetch the stream makes leaves a listener reaching the served request on its signal.
      globalThis.fetch = Object.assign(
        (input: RequestInfo | URL, init?: RequestInit) => {
          const served = servedRequest.getStore();
          if (served) init?.signal?.addEventListener('abort', () => void served.url);
          return realFetch(input, init);
        },
        { preconnect: realFetch.preconnect },
      );
      const transport = new ConnectTransport(`http://localhost:${backend.port}`, 'test.v1');
      const retained = await retainedAfterServing((served) =>
        servedRequest.run(
          served,
          () =>
            new Promise<void>((resolve, reject) => {
              const stream = transport.stream('/test.v1.ItemsService/WatchItems', { signal: served.signal });
              stream.onComplete(() => resolve());
              stream.onError(reject);
            }),
        ),
      );
      expect(retained).toBeLessThan(SERVED / 5);
    });
  });
});
