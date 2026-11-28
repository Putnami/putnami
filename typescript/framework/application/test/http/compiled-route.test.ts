import { afterEach, describe, expect, it } from 'bun:test';
import {
  HttpException,
  Logger,
  NotAcceptableException,
  resetDefaultLogger,
  resetLoggerConfig,
  setLogLevel,
  setRootLogger,
} from '@putnami/runtime';
import { resolve } from '@putnami/runtime/inject';
import { MemorySink } from '@putnami/runtime/testing';
import { api } from '../../src/api';
import { endpoint } from '../../src/api/route/endpoint';
import { application } from '../../src/application';
import { type CompiledAnswer, CompiledRequest } from '../../src/http/compiled-route';
import type { HttpRequestContextInternal } from '../../src/http/http-context.type';
import { HttpResponse } from '../../src/http/http-response';
import { HttpPlugin, http } from '../../src/http/http.plugin';
import { LoggerMiddleware } from '../../src/http/logger.middleware';
import type { MiddlewareForm } from '../../src/http/middleware-form';

const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms));

type BunHandler = (req: Request, server: unknown) => Response | Promise<Response>;

/** Starts `plugin` and keeps the routes table it handed Bun. */
async function startCapturing(buildApp: () => ReturnType<typeof application>, plugin: HttpPlugin) {
  // The routes table the plugin hands Bun, as it builds it.
  const prototype = HttpPlugin.prototype as unknown as { buildNativeRoutes: (...args: unknown[]) => unknown };
  const build = prototype.buildNativeRoutes;
  let routes: Record<string, Record<string, BunHandler>> = {};
  prototype.buildNativeRoutes = function (this: unknown, ...args: unknown[]) {
    const built = build.apply(this, args) as typeof routes | undefined;
    routes = built ?? {};
    return built;
  };
  const app = buildApp();
  try {
    await app.start();
  } finally {
    prototype.buildNativeRoutes = build;
  }
  const server = plugin.getServer();
  return {
    app,
    base: `http://localhost:${server?.port}`,
    /** Calls the handler Bun runs for `method` `pattern`, as Bun would. */
    call(method: string, pattern: string, url: string, params?: Record<string, string>, init?: RequestInit) {
      const req = new Request(url, { method, ...init });
      if (params) (req as Request & { params: Record<string, string> }).params = params;
      const handler = routes[pattern]?.[method];
      if (!handler) throw new Error(`no native route ${method} ${pattern}`);
      return handler(req, server);
    },
  };
}

describe('CompiledRequest', () => {
  const ctx = {} as HttpRequestContextInternal;
  const answerError = (_ctx: unknown, _served: unknown, error: unknown) =>
    new HttpResponse(String((error as Error).message), { status: 599 });
  const run = (forms: MiddlewareForm<unknown>[], inner: () => HttpResponse | undefined): CompiledAnswer =>
    new CompiledRequest(ctx, forms, undefined, answerError).run(
      {
        runRoute: (_ctx, _handler, continuation) => {
          let response: HttpResponse | undefined;
          try {
            response = inner();
          } catch (error) {
            return continuation.failed(error);
          }
          return continuation.answered(response);
        },
      },
      () => undefined,
    );

  it('enters outermost first, leaves innermost first, and answers at once', () => {
    const calls: string[] = [];
    const form = (name: string): MiddlewareForm<unknown> => ({
      enter: () => {
        calls.push(`enter ${name}`);
        return name;
      },
      leave: (_c, response, state) => {
        calls.push(`leave ${state}`);
        return response;
      },
    });
    const ok = new HttpResponse('ok');
    const answer = run([form('a'), form('b')], () => ok);
    expect(answer).toBe(ok);
    expect(calls).toEqual(['enter a', 'enter b', 'leave b', 'leave a']);
  });

  it('hands an error thrown in a leave to the forms outside it, then answers it', () => {
    const failed: string[] = [];
    const answer = run(
      [
        { fail: (_c, error) => failed.push(`outer ${(error as Error).message}`) },
        {
          leave: () => {
            throw new Error('leave threw');
          },
          fail: () => failed.push('own'),
        },
      ],
      () => new HttpResponse('ok'),
    ) as HttpResponse;
    expect(failed).toEqual(['outer leave threw']);
    expect(answer.status).toBe(599);
  });

  it('replaces the error with the one a fail throws', () => {
    const seen: string[] = [];
    const answer = run(
      [
        { fail: (_c, error) => seen.push((error as Error).message) },
        {
          fail: () => {
            throw new Error('replaced');
          },
        },
      ],
      () => {
        throw new Error('handler failed');
      },
    ) as HttpResponse;
    expect(seen).toEqual(['replaced']);
    expect(answer.status).toBe(599);
  });

  it('runs no hook of a form that lets the request pass', () => {
    let hooks = 0;
    const ok = new HttpResponse('ok');
    const answer = run(
      [
        {
          passes: () => true,
          enter: () => hooks++,
          leave: () => {
            hooks++;
            return undefined;
          },
          fail: () => {
            hooks++;
          },
        },
      ],
      () => ok,
    );
    expect(answer).toBe(ok);
    expect(hooks).toBe(0);
  });

  it('answers early, at once or through a promise, past the inner forms and the handler', async () => {
    const refused = new HttpResponse('refused', { status: 403 });
    for (const early of [refused, Promise.resolve(refused)]) {
      const calls: string[] = [];
      const answer = await run(
        [
          { leave: (_c, response) => (calls.push('outer leave'), response) },
          { answer: () => early },
          { enter: () => calls.push('inner enter') },
        ],
        () => {
          calls.push('handler');
          return new HttpResponse('ok');
        },
      );
      expect(answer).toBe(refused);
      expect(calls).toEqual(['outer leave']);
    }
  });
});

describe('a route Bun matches, compiled', () => {
  let stop: (() => Promise<void>) | undefined;
  afterEach(async () => {
    await stop?.();
    stop = undefined;
    resetDefaultLogger();
    resetLoggerConfig();
  });

  it('answers a synchronous handler with the Response itself, secured, logged, and released from its deadline', async () => {
    const sink = new MemorySink();
    setRootLogger(new Logger([sink]));
    setLogLevel('debug');
    const signals: AbortSignal[] = [];
    const plugin = http({ port: 0, requestTimeoutMs: 200 }).use(LoggerMiddleware());
    plugin.get('/items/[id]', (ctx) => {
      signals.push(ctx.signal as AbortSignal);
      return { id: ctx.params?.['id'] };
    });
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();

    const res = served.call('GET', '/items/:id', `${served.base}/items/7`, { id: '7' });
    expect(res).toBeInstanceOf(Response);
    const response = res as Response;
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ id: '7' });
    expect(response.headers.get('X-Content-Type-Options')).toBe('nosniff');
    expect(response.headers.get('Content-Type')).toBe('application/json');

    const access = sink.entries.find((entry) => entry.message === '[GET] /items/[id]');
    expect(access?.level).toBe('debug');
    expect((access?.context?.['http'] as Record<string, unknown> | undefined)?.['status']).toBe(200);

    // Past the deadline: a request still held to it would have its signal aborted.
    await sleep(300);
    expect(signals[0]?.aborted).toBe(false);

    const listed = sink.entries.find((entry) => entry.message === 'compiled routes');
    expect((listed?.data?.[0] as Record<string, unknown> | undefined)?.['compiled']).toEqual(['GET /items/[id]']);
  });

  it('answers 408 at the deadline of an asynchronous handler', async () => {
    const plugin = http({ port: 0, requestTimeoutMs: 100 });
    plugin.get('/slow', async () => {
      await sleep(300);
      return { late: true };
    });
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();
    const res = served.call('GET', '/slow', `${served.base}/slow`);
    expect(res).toBeInstanceOf(Promise);
    const response = await res;
    expect(response.status).toBe(408);
    expect(await response.json()).toEqual({ error: 'Request Timeout', statusCode: 408 });
  });

  it('refuses a cross-site state change with the secure headers, and serves a same-origin one', async () => {
    const plugin = http({ port: 0 });
    plugin.post('/items', () => ({ created: true }));
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();
    const refused = await fetch(`${served.base}/items`, {
      method: 'POST',
      headers: { Origin: 'https://evil.example', 'Sec-Fetch-Site': 'cross-site' },
    });
    expect(refused.status).toBe(403);
    expect(await refused.json()).toEqual({ error: 'Cross-origin request blocked' });
    expect(refused.headers.get('X-Content-Type-Options')).toBe('nosniff');
    const accepted = await fetch(`${served.base}/items`, {
      method: 'POST',
      headers: { 'Sec-Fetch-Site': 'same-origin' },
    });
    expect(accepted.status).toBe(200);
  });

  it('answers a thrown HttpException, the error handler and a server error as the chain does', async () => {
    const sink = new MemorySink();
    setRootLogger(new Logger([sink]));
    const plugin = http({
      port: 0,
      onError: (error) => (error instanceof Error && error.message === 'handled' ? { handled: true } : undefined),
    }).use(LoggerMiddleware());
    plugin.get('/conflict', () => {
      throw new HttpException('taken', 409);
    });
    plugin.get('/handled', () => {
      throw new Error('handled');
    });
    plugin.get('/boom', () => {
      throw new Error('boom');
    });
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();

    const conflict = await served.call('GET', '/conflict', `${served.base}/conflict`);
    expect(conflict.status).toBe(409);
    expect(conflict.headers.get('X-Content-Type-Options')).toBe('nosniff');
    const handled = await served.call('GET', '/handled', `${served.base}/handled`);
    expect(handled.status).toBe(500);
    expect(await handled.json()).toEqual({ handled: true });
    const boom = await served.call('GET', '/boom', `${served.base}/boom`);
    expect(boom.status).toBe(500);
    const record = sink.entries.find((entry) => entry.message === '[GET] /boom');
    expect(record?.level).toBe('error');
    expect(record?.error?.message).toBe('boom');
  });

  it('leaves unanswered a request a response callback refuses the Accept of', async () => {
    const plugin = http({ port: 0 });
    plugin.get('/refused', (ctx) => {
      (ctx as HttpRequestContextInternal).__callbacks = [
        () => {
          throw new NotAcceptableException('no representation');
        },
      ];
      return { ok: true };
    });
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();
    const res = await served.call('GET', '/refused', `${served.base}/refused`);
    expect(res.status).toBe(404);
    expect(await res.json()).toEqual({ error: 'Not Found', statusCode: 404 });
  });

  it('answers an endpoint that returned nothing with 204, and closes its request scope first', async () => {
    class Resource {}
    let closed = 0;
    const routes = api({ autoScan: false })
      .register('/empty', { GET: endpoint().handle(() => undefined) }, 'GET')
      .register(
        '/scoped',
        {
          GET: endpoint().handle(() => {
            resolve(Resource);
            return { scoped: true };
          }),
        },
        'GET',
      );
    const plugin = http({ port: 0 });
    const served = await startCapturing(
      () =>
        application()
          .provide(Resource, () => new Resource(), {
            scope: 'scoped',
            onClose: async () => {
              await sleep(10);
              closed++;
            },
          })
          .use(plugin)
          .use(routes),
      plugin,
    );
    stop = () => served.app.stop();
    const empty = served.call('GET', '/empty', `${served.base}/empty`);
    expect(empty).toBeInstanceOf(Response);
    expect((empty as Response).status).toBe(204);
    const scoped = await served.call('GET', '/scoped', `${served.base}/scoped`);
    expect(closed).toBe(1);
    expect(await scoped.json()).toEqual({ scoped: true });
  });

  it('keeps the middleware chain on a route a middleware without a form acts on, and says why', async () => {
    const sink = new MemorySink();
    setRootLogger(new Logger([sink]));
    setLogLevel('debug');
    const plugin = http({ port: 0 });
    plugin.use(async function stamp(_ctx, next) {
      const res = await next();
      return res?.setHeader('X-Stamp', '1');
    });
    plugin.get('/stamped', () => ({ ok: true }));
    const served = await startCapturing(() => application().use(plugin), plugin);
    stop = () => served.app.stop();
    const res = served.call('GET', '/stamped', `${served.base}/stamped`);
    expect(res).toBeInstanceOf(Promise);
    expect((await res).headers.get('X-Stamp')).toBe('1');
    const listed = sink.entries.find((entry) => entry.message === 'compiled routes');
    expect((listed?.data?.[0] as Record<string, unknown> | undefined)?.['compiled']).toEqual([]);
    const chained = (listed?.data?.[0] as Record<string, unknown> | undefined)?.['chained'] as {
      route: string;
      reason: string;
    }[];
    expect(chained[0]?.route).toBe('/stamped');
    expect(chained[0]?.reason).toContain('(stamp) has no synchronous form');
  });
});
