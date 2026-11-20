import { afterEach, describe, expect, it } from 'bun:test';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';

// A Connect unary call runs on the HTTP request it arrived in, so the request
// timeout stops its handler as it stops a REST handler.

describe('gRPC request timeout', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  it('stops a unary handler at the request timeout, answers 408 and logs no error', async () => {
    const reported: unknown[] = [];
    let handlerSignal: AbortSignal | undefined;
    const httpPlugin: HttpPlugin = http({
      port: 0,
      requestTimeoutMs: 100,
      onError: (error) => {
        reported.push(error);
        return undefined;
      },
    });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/waits',
      {
        GET: endpoint()
          .returns({ value: String })
          .handle(async (ctx) => {
            handlerSignal = ctx.signal;
            // Waits on a dependency that never answers; only the signal ends the wait.
            await new Promise((_, reject) => ctx.signal?.addEventListener('abort', () => reject(ctx.signal?.reason)));
            return { value: 'never' };
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
    const memory = new MemoryLogger();
    setRootLogger(memory);

    try {
      const response = await fetch(`http://localhost:${httpPlugin.getServer()?.port}/test.v1.WaitsService/ListWaits`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'connect-protocol-version': '1' },
        body: JSON.stringify({}),
      });
      await new Promise((resolve) => setTimeout(resolve, 50));

      expect(response.status).toBe(408);
      expect(handlerSignal?.aborted).toBe(true);
      expect(reported).toEqual([]);
      expect(memory.entries.filter((entry) => entry.level === 'error')).toEqual([]);
    } finally {
      resetDefaultLogger();
    }
  });
});
