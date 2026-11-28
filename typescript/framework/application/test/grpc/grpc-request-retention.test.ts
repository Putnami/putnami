import { afterEach, describe, expect, it } from 'bun:test';
import { heapStats } from 'bun:jsc';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';

// Bun keeps a signal AbortSignal.any builds, listeners included, alive while
// one of its sources is alive and not aborted. A unary call that settles
// aborts neither its deadline nor its HTTP request, so a listener on the
// handler's signal that reaches the request would keep the request alive.

const SERVED = 50;

async function retainedRequests(): Promise<number> {
  for (let i = 0; i < 5; i++) {
    Bun.gc(true);
    // biome-ignore lint/performance/noAwaitInLoops: each collection lets finalizers run before the next
    await Bun.sleep(10);
  }
  return heapStats().objectTypeCounts['Request'] ?? 0;
}

describe('gRPC unary dispatch signal', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  it('retains no served request once a call with a deadline settles, whatever listener its handler leaves', async () => {
    const httpPlugin: HttpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/items',
      {
        GET: endpoint()
          .returns({ value: String })
          .handle((ctx) => {
            // A listener that reaches the request and is never removed.
            ctx.signal?.addEventListener('abort', () => void ctx.req.url);
            return { value: 'ok' };
          }),
      },
      'GET',
    );
    app = application()
      .use(httpPlugin)
      .use(apiPlugin)
      .use(proto({ packageName: 'test.v1', output: false }))
      .use(grpc());
    await app.start();
    const url = `http://localhost:${httpPlugin.getServer()?.port}/test.v1.ItemsService/ListItems`;

    const before = await retainedRequests();
    for (let i = 0; i < SERVED; i++) {
      // biome-ignore lint/performance/noAwaitInLoops: requests are served one after the other
      const response = await fetch(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'connect-protocol-version': '1', 'connect-timeout-ms': '60000' },
        body: '{}',
      });
      expect(response.status).toBe(200);
      await response.text();
    }
    await Bun.sleep(20);

    expect((await retainedRequests()) - before).toBeLessThan(SERVED / 5);
  });
});
