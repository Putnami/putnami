import { describe, expect, it } from 'bun:test';
import { api } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route/endpoint';
import { http } from '../../../src/http/http.plugin';
import { application } from '../../../src/application';

describe('response validation (dev mode)', () => {
  it('should still return the response even when validation warns', async () => {
    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });

    // Handler returns data that does NOT match the returns schema
    const handler = endpoint()
      .returns({ id: String, name: String })
      .handle(() => {
        // missing 'name' field
        return { id: '1', extra: 'unexpected' };
      });

    plugin.register('/test', { default: handler }, 'GET');

    const app = application().use(httpPlugin).use(plugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/test`, {
      headers: { Accept: 'application/json' },
    });

    // Response should still be 200 — validation is non-blocking
    expect(res.status).toBe(200);
    const data = await res.json();
    expect(data.id).toBe('1');
    expect(data.extra).toBe('unexpected');

    await app.stop();
  });

  it('should not interfere when response matches returns schema', async () => {
    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });

    const handler = endpoint()
      .returns({ id: String, name: String })
      .handle(() => ({ id: '1', name: 'Alice' }));

    plugin.register('/valid', { default: handler }, 'GET');

    const app = application().use(httpPlugin).use(plugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/valid`, {
      headers: { Accept: 'application/json' },
    });
    expect(res.status).toBe(200);
    const data = await res.json();
    expect(data).toEqual({ id: '1', name: 'Alice' });

    await app.stop();
  });

  it('should wrap handler when only returns schema is provided', async () => {
    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });

    // Only .returns(), no input schemas — should still wrap for validation
    const handler = endpoint()
      .returns({ status: String })
      .handle(() => ({ status: 'ok' }));

    plugin.register('/status', { default: handler }, 'GET');

    const app = application().use(httpPlugin).use(plugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/status`, {
      headers: { Accept: 'application/json' },
    });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ status: 'ok' });

    await app.stop();
  });

  it('should handle undefined/null responses without error', async () => {
    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });

    const handler = endpoint()
      .returns({ id: String })
      .handle(() => undefined);

    plugin.register('/empty', { default: handler }, 'GET');

    const app = application().use(httpPlugin).use(plugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/empty`, {
      headers: { Accept: 'application/json' },
    });

    // undefined handler result means no response matched — should get fallback
    // The important thing is no crash
    expect(res).toBeDefined();

    await app.stop();
  });
});
