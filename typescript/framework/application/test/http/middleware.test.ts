import { describe, expect, it } from 'bun:test';
import { type HttpMiddleware, http } from '../../src/http';
import { application } from '../../src/application';

describe('Http Middleware', () => {
  it('should execute middleware', async () => {
    const executed: string[] = [];
    const middleware: HttpMiddleware = async (_context, next) => {
      executed.push('start');
      const res = await next();
      executed.push('end');
      return res;
    };

    const plugin = http({ port: 0 }).use(middleware);
    plugin.get('/', () => 'hello');

    const app = application().use(plugin);
    await app.start();

    const server = plugin.getServer();
    if (!server) throw new Error('Server not started');

    const port = server.port;
    const res = await fetch(`http://localhost:${port}/`);
    expect(await res.text()).toBe('hello');

    expect(executed).toEqual(['start', 'end']);

    await app.stop();
  });

  it('should stack middlewares', async () => {
    const executed: string[] = [];
    const m1: HttpMiddleware = async (_c, n) => {
      executed.push('m1-start');
      const r = await n();
      executed.push('m1-end');
      return r;
    };
    const m2: HttpMiddleware = async (_c, n) => {
      executed.push('m2-start');
      const r = await n();
      executed.push('m2-end');
      return r;
    };

    const plugin = http({ port: 0 }).use(m1).use(m2);
    plugin.get('/', () => 'ok');

    const app = application().use(plugin);
    await app.start();

    const port = plugin.getServer()?.port;
    await fetch(`http://localhost:${port}/`);

    expect(executed).toEqual(['m1-start', 'm2-start', 'm2-end', 'm1-end']);

    await app.stop();
  });

  it('should have route available in middleware', async () => {
    let capturedRoute: string | undefined;
    const middleware: HttpMiddleware = async (ctx, next) => {
      capturedRoute = ctx.route;
      return next();
    };

    const plugin = http({ port: 0 }).use(middleware);
    plugin.get('/users/[id]', () => 'ok');

    const app = application().use(plugin);
    await app.start();

    const port = plugin.getServer()?.port;
    await fetch(`http://localhost:${port}/users/123`);

    expect(capturedRoute).toBe('/users/[id]');

    await app.stop();
  });

  it('should handle ctx.throw()', async () => {
    const plugin = http({ port: 0 });
    plugin.get('/protected', (ctx) => {
      ctx.throw(401, 'Unauthorized');
      return undefined; // satisfy RouteHandler return type
    });

    const app = application().use(plugin);
    await app.start();

    const port = plugin.getServer()?.port;
    const res = await fetch(`http://localhost:${port}/protected`);

    expect(res.status).toBe(401);

    await app.stop();
  });
});
