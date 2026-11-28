import { afterEach, describe, expect, it } from 'bun:test';
import { HttpException } from '@putnami/runtime';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { HttpPlugin, http } from '../../src/http/http.plugin';
import { middlewareFormOf } from '../../src/http/middleware-form';
import { activeTelemetryContext } from '../../src/telemetry/client-telemetry';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';
import { setCollector } from '../../src/telemetry/telemetry.utils';
import { requestCounts } from './http-server-series';

const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms));

type BunHandler = (req: Request, server: unknown) => Response | Promise<Response>;

describe('the telemetry middleware on a compiled route', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    setCollector(undefined);
  });

  /** Starts a server with telemetry outermost, and keeps the routes table it handed Bun. */
  async function serve(register: (plugin: HttpPlugin) => void, requestTimeoutMs?: number) {
    const collector = new TelemetryCollector();
    setCollector(collector);
    const plugin = http({ port: 0, requestTimeoutMs }).useOutermost(TelemetryMiddleware(collector));
    register(plugin);
    const prototype = HttpPlugin.prototype as unknown as { buildNativeRoutes: (...args: unknown[]) => unknown };
    const build = prototype.buildNativeRoutes;
    let routes: Record<string, Record<string, BunHandler>> = {};
    prototype.buildNativeRoutes = function (this: unknown, ...args: unknown[]) {
      const built = build.apply(this, args) as typeof routes | undefined;
      routes = built ?? {};
      return built;
    };
    app = application().use(plugin);
    try {
      await app.start();
    } finally {
      prototype.buildNativeRoutes = build;
    }
    const server = plugin.getServer();
    const base = `http://localhost:${server?.port}`;
    const call = (pattern: string, path: string, params?: Record<string, string>, headers?: HeadersInit) => {
      const handler = routes[pattern]?.['GET'];
      if (!handler) throw new Error(`no native route GET ${pattern}`);
      const req = new Request(`${base}${path}`, { headers });
      // Bun sets the params of the pattern it matched.
      if (params) (req as Request & { params: Record<string, string> }).params = params;
      return handler(req, server);
    };
    return { collector, call };
  }

  it('has a form, so a service with telemetry compiles its routes', () => {
    expect(middlewareFormOf(TelemetryMiddleware(new TelemetryCollector()))).toBeDefined();
  });

  it('counts a request answered at once under its route pattern and status, with no promise', async () => {
    const { collector, call } = await serve((plugin) => plugin.get('/users/[id]', () => ({ ok: true })));
    const res = call('/users/:id', '/users/7', { id: '7' });
    expect(res).toBeInstanceOf(Response);
    expect((res as Response).status).toBe(200);
    expect(requestCounts(collector.drainAll())).toEqual([
      { method: 'GET', route: '/users/[id]', status: 200, count: 1 },
    ]);
  });

  it('counts a thrown HttpException under its status and a server error as 500', async () => {
    const { collector, call } = await serve((plugin) => {
      plugin.get('/conflict', () => {
        throw new HttpException('taken', 409);
      });
      plugin.get('/boom', () => {
        throw new Error('boom');
      });
    });
    expect((await call('/conflict', '/conflict')).status).toBe(409);
    expect((await call('/boom', '/boom')).status).toBe(500);
    const counts = requestCounts(collector.drainAll()).sort((a, b) => a.status - b.status);
    expect(counts).toEqual([
      { method: 'GET', route: '/conflict', status: 409, count: 1 },
      { method: 'GET', route: '/boom', status: 500, count: 1 },
    ]);
  });

  it('counts a request that outlived its deadline as 408', async () => {
    const { collector, call } = await serve(
      (plugin) =>
        plugin.get('/slow', async (ctx) => {
          await sleep(200);
          if (ctx.signal?.aborted) throw ctx.signal.reason;
          return { late: true };
        }),
      50,
    );
    expect((await call('/slow', '/slow')).status).toBe(408);
    await sleep(250);
    expect(requestCounts(collector.drainAll())).toEqual([{ method: 'GET', route: '/slow', status: 408, count: 1 }]);
  });

  it('ends a server span under the incoming trace, with the trace context visible to the handler', async () => {
    const traceId = '4bf92f3577b34da6a3ce929d0e0e4736';
    const parentSpanId = '00f067aa0ba902b7';
    let seen: string | undefined;
    const { collector, call } = await serve((plugin) =>
      plugin.get('/users/[id]', () => {
        seen = activeTelemetryContext()?.traceId;
        return { ok: true };
      }),
    );
    const res = call('/users/:id', '/users/7', { id: '7' }, { traceparent: `00-${traceId}-${parentSpanId}-01` });
    expect(res).toBeInstanceOf(Response);
    expect(seen).toBe(traceId);
    const spans = collector.drainSpans().filter((span) => span.kind === 2);
    expect(spans).toHaveLength(1);
    expect(spans[0]).toMatchObject({ name: 'GET /users/[id]', kind: 2, traceId, parentSpanId });
    expect(spans[0]?.attributes['http.response.status_code']).toBe(200);
  });

  it('marks the server span of a failed request with its status', async () => {
    const { collector, call } = await serve(
      (plugin) => {
        plugin.get('/conflict', () => {
          throw new HttpException('taken', 409);
        });
        plugin.get('/boom', () => {
          throw new Error('boom');
        });
        plugin.get('/slow', async (ctx) => {
          await sleep(200);
          if (ctx.signal?.aborted) throw ctx.signal.reason;
          return { late: true };
        });
      },
      50,
    );
    await call('/conflict', '/conflict');
    await call('/boom', '/boom');
    await call('/slow', '/slow');
    await sleep(250);
    const statuses = collector
      .drainSpans()
      .filter((span) => span.kind === 2)
      .map((span) => [span.name, span.statusMessage])
      .sort();
    expect(statuses).toEqual([
      ['GET /boom', 'http.500'],
      ['GET /conflict', 'http.409'],
      ['GET /slow', 'http.408'],
    ]);
  });
});
