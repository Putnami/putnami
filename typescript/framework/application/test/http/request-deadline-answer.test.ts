import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resolve } from '@putnami/runtime/inject';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import type { HttpResponse } from '../../src/http/http-response';
import { type HttpPlugin, http } from '../../src/http/http.plugin';

const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms));

/** A request-scoped dependency whose scope closes asynchronously. */
class RequestResource {}

describe('the request deadline and the answer of a request', () => {
  let app: Application;
  let plugin: HttpPlugin;
  let baseUrl: string;
  let signals: AbortSignal[];
  let closed: number;
  let handlerEnded: boolean;

  beforeEach(async () => {
    signals = [];
    closed = 0;
    handlerEnded = false;
    plugin = http({ port: 0, requestTimeoutMs: 300 });
    plugin.get('/fast/[n]', (ctx) => {
      signals.push(ctx.signal as AbortSignal);
      return { n: ctx.params?.['n'] };
    });
    plugin.get('/scoped', (ctx) => {
      signals.push(ctx.signal as AbortSignal);
      resolve(RequestResource);
      return { scoped: true };
    });
    plugin.get('/scoped-slow', async () => {
      resolve(RequestResource);
      await sleep(450);
      handlerEnded = true;
      return { late: true };
    });
    plugin.use(async (ctx, next) => {
      if (ctx.path() !== 'broken') return next();
      signals.push(ctx.signal as AbortSignal);
      // An answer whose native response cannot be built.
      return {
        get() {
          throw new Error('no response');
        },
      } as unknown as HttpResponse;
    });
    app = application()
      .provide(RequestResource, () => new RequestResource(), {
        scope: 'scoped',
        onClose: async () => {
          await sleep(20);
          closed++;
        },
      })
      .use(plugin);
    await app.start();
    baseUrl = `http://localhost:${plugin.getServer()?.port}`;
  });

  afterEach(async () => {
    await app.stop();
  });

  it('answers each request in time with its own response, and its deadline no longer holds it', async () => {
    const answers = await Promise.all(
      Array.from({ length: 32 }, async (_, n) => {
        const res = await fetch(`${baseUrl}/fast/${n}`);
        return { status: res.status, body: await res.json() };
      }),
    );
    expect(answers).toEqual(Array.from({ length: 32 }, (_, n) => ({ status: 200, body: { n: String(n) } })));
    // Past the deadline: a request still queued on it would have its signal aborted.
    await sleep(400);
    expect(signals).toHaveLength(32);
    expect(signals.filter((signal) => signal.aborted)).toEqual([]);
  });

  it('answers once the request scope closed, and its deadline no longer holds it', async () => {
    const res = await fetch(`${baseUrl}/scoped`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ scoped: true });
    expect(closed).toBe(1);
    await sleep(400);
    expect(signals[0]?.aborted).toBe(false);
  });

  it('fails a request whose response cannot be built, and its deadline no longer holds it', async () => {
    const res = await fetch(`${baseUrl}/broken`);
    expect(res.status).toBe(500);
    await sleep(400);
    expect(signals).toHaveLength(1);
    expect(signals[0]?.aborted).toBe(false);
  });

  it('answers 408 at the deadline and closes the request scope once its handler ended', async () => {
    const res = await fetch(`${baseUrl}/scoped-slow`);
    expect(res.status).toBe(408);
    expect(await res.json()).toEqual({ error: 'Request Timeout', statusCode: 408 });
    expect(handlerEnded).toBe(false);
    expect(closed).toBe(0);
    await sleep(400);
    expect(handlerEnded).toBe(true);
    expect(closed).toBe(1);
  });
});
