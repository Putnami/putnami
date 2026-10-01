import { describe, expect, it } from 'bun:test';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';

/** Read Bun's native route params off the raw request (typed as Request for users). */
function nativeParams(req: Request): Record<string, string> {
  return (req as unknown as { params?: Record<string, string> }).params ?? {};
}

describe('native (pipeline-bypassing) routes', () => {
  it('serves a static native route and bypasses the pipeline (no security headers)', async () => {
    const plugin = http({ port: 0 }); // secure by default
    plugin.native('/healthz', HttpResponse.json({ status: 'ok' }));
    plugin.get('/normal', () => HttpResponse.json({ status: 'ok' }));
    const app = application().use(plugin);
    await app.start();
    const base = `http://localhost:${plugin.getServer()?.port}`;

    const native = await fetch(`${base}/healthz`);
    expect(native.status).toBe(200);
    expect(await native.json()).toEqual({ status: 'ok' });
    // Pipeline bypassed → none of the secure-by-default headers are present.
    expect(native.headers.get('X-Frame-Options')).toBeNull();

    // A normal pipeline route on the same server still gets the security headers.
    const normal = await fetch(`${base}/normal`);
    expect(normal.headers.get('X-Frame-Options')).toBe('DENY');
    await normal.arrayBuffer();

    await app.stop();
  });

  it('serves a bare function native route', async () => {
    const plugin = http({ port: 0 });
    plugin.native('GET', '/version', () => HttpResponse.json({ version: '1.0' }));
    plugin.native('/raw', new Response('RAW'));
    const app = application().use(plugin);
    await app.start();
    const base = `http://localhost:${plugin.getServer()?.port}`;

    expect(await (await fetch(`${base}/version`)).json()).toEqual({ version: '1.0' });
    expect(await (await fetch(`${base}/raw`)).text()).toBe('RAW');

    await app.stop();
  });

  it('serves a method-specific native route', async () => {
    const plugin = http({ port: 0 });
    plugin.native('POST', '/submit', () => HttpResponse.json({ ok: true }));
    const app = application().use(plugin);
    await app.start();
    const base = `http://localhost:${plugin.getServer()?.port}`;

    const res = await fetch(`${base}/submit`, { method: 'POST' });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });

    await app.stop();
  });

  it('maps [param] paths to Bun native params', async () => {
    const plugin = http({ port: 0 });
    plugin.native('GET', '/echo/[id]', (req) => HttpResponse.json({ id: nativeParams(req)['id'] }));
    const app = application().use(plugin);
    await app.start();
    const base = `http://localhost:${plugin.getServer()?.port}`;

    expect(await (await fetch(`${base}/echo/abc-123`)).json()).toEqual({ id: 'abc-123' });

    await app.stop();
  });

  it('falls through to the pipeline for non-native paths', async () => {
    const plugin = http({ port: 0 });
    plugin.native('/healthz', HttpResponse.json({ status: 'ok' }));
    plugin.get('/pipeline', () => HttpResponse.json({ via: 'pipeline' }));
    const app = application().use(plugin);
    await app.start();
    const base = `http://localhost:${plugin.getServer()?.port}`;

    expect(await (await fetch(`${base}/pipeline`)).json()).toEqual({ via: 'pipeline' });
    // Unknown path → the pipeline's 404, proving fetch() is still the fallback.
    const miss = await fetch(`${base}/nope`);
    expect(miss.status).toBe(404);
    await miss.arrayBuffer();

    await app.stop();
  });

  it('propagates native routes through merge()', async () => {
    const sub = http();
    sub.native('/sub-health', HttpResponse.json({ ok: true }));
    const root = http({ port: 0 });
    root.merge(sub);
    const app = application().use(root);
    await app.start();
    const base = `http://localhost:${root.getServer()?.port}`;

    expect(await (await fetch(`${base}/sub-health`)).json()).toEqual({ ok: true });

    await app.stop();
  });
});
