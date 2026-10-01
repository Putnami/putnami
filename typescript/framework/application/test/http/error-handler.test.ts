import { afterEach, describe, expect, it } from 'bun:test';
import { HttpException } from '@putnami/runtime';
import { restoreEnv } from '@putnami/utils';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { type HttpPlugin, http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';

// Realistic browser Accept header — includes text/html AND */* so routes without
// explicit accept types still match (the router requires */* for untyped routes).
const HTML_ACCEPT = { headers: { Accept: 'text/html,application/xhtml+xml,*/*;q=0.8' } };

describe('onError handler', () => {
  let app: Application;
  let plugin: HttpPlugin;
  let baseUrl: string;

  afterEach(async () => {
    await app.stop();
  });

  async function start(pluginInstance: HttpPlugin) {
    plugin = pluginInstance;
    app = application().use(plugin);
    await app.start();
    baseUrl = `http://localhost:${plugin.getServer()?.port}`;
  }

  it('sync error handler returns a custom body', async () => {
    const p = http({
      port: 0,
      onError: (error) => ({ customError: String(error) }),
    });
    p.get('/fail', () => {
      throw new Error('boom');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(500);
    const json = await res.json();
    expect(json.customError).toBe('Error: boom');
  });

  it('async error handler is awaited and returns a custom body', async () => {
    const p = http({
      port: 0,
      onError: async (error) => {
        // Simulate async work (e.g. reporting to Sentry)
        await new Promise((r) => setTimeout(r, 5));
        return { asyncError: String(error) };
      },
    });
    p.get('/fail', () => {
      throw new Error('async-boom');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(500);
    const json = await res.json();
    expect(json.asyncError).toBe('Error: async-boom');
  });

  it('error handler returning undefined falls through to default', async () => {
    const p = http({
      port: 0,
      onError: () => undefined,
    });
    p.get('/fail', () => {
      throw new HttpException('Not Found', 404);
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(404);
  });

  it('error handler that throws logs and falls through', async () => {
    const p = http({
      port: 0,
      onError: () => {
        throw new Error('handler-crash');
      },
    });
    p.get('/fail', () => {
      throw new HttpException('Server Error', 500);
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    // Falls through to default HttpException handling
    expect(res.status).toBe(500);
  });

  it('error handler receives the HttpRequestContext', async () => {
    let receivedPath: string | undefined;
    const p = http({
      port: 0,
      onError: (_error, ctx) => {
        receivedPath = ctx.path();
        return { path: ctx.path() };
      },
    });
    p.get('/ctx-check', () => {
      throw new Error('test');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/ctx-check`);
    expect(res.status).toBe(500);
    expect(receivedPath).toBe('ctx-check');
  });

  it('without error handler, HttpException produces correct status', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new HttpException('Bad Request', 400);
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(400);
  });

  it('without error handler, HttpResponse thrown is returned as-is', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new HttpResponse('custom error page', { status: 503 });
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(503);
    expect(await res.text()).toBe('custom error page');
  });
});

describe('dev error page', () => {
  let app: Application;
  let plugin: HttpPlugin;
  let baseUrl: string;

  afterEach(async () => {
    await app.stop();
  });

  async function start(pluginInstance: HttpPlugin) {
    plugin = pluginInstance;
    app = application().use(plugin);
    await app.start();
    baseUrl = `http://localhost:${plugin.getServer()?.port}`;
  }

  it('returns HTML error page when Accept: text/html in development', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('dev-boom');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
    expect(res.status).toBe(500);
    expect(res.headers.get('content-type')).toContain('text/html');
    const body = await res.text();
    expect(body).toContain('dev-boom');
    expect(body).toContain('Stack Trace');
    expect(body).toContain('Request');
  });

  it('returns JSON with error details in dev mode by default', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('json-error');
    });
    await start(p);

    // Default fetch (no Accept header → */*) triggers JSON error, not HTML overlay
    const res = await fetch(`${baseUrl}/fail`);
    expect(res.status).toBe(500);
    expect(res.headers.get('content-type')).toContain('application/json');
    const body = await res.json();
    // In dev mode, error details are included by default
    expect(body.error).toBe('json-error');
    expect(typeof body.stack).toBe('string');
  });

  it('suppresses error details when PUTNAMI_DEV_ERRORS=false', async () => {
    const previousVal = process.env.PUTNAMI_DEV_ERRORS;
    process.env.PUTNAMI_DEV_ERRORS = 'false';
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('hidden-error');
    });
    await start(p);

    try {
      const res = await fetch(`${baseUrl}/fail`);
      expect(res.status).toBe(500);
      const body = await res.json();
      expect(body.error).toBe('Internal Server Error');
      expect(body.stack).toBeUndefined();
    } finally {
      restoreEnv('PUTNAMI_DEV_ERRORS', previousVal);
    }
  });

  it('suppresses the HTML dev overlay when PUTNAMI_DEV_ERRORS=false', async () => {
    const previousVal = process.env.PUTNAMI_DEV_ERRORS;
    process.env.PUTNAMI_DEV_ERRORS = 'false';
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('hidden-html-error');
    });
    await start(p);

    try {
      // Even with a browser Accept header, the kill-switch must suppress the
      // stack-trace / source overlay (regression: HTML path ignored the flag).
      const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
      expect(res.status).toBe(500);
      expect(res.headers.get('content-type')).not.toContain('text/html');
      const body = await res.text();
      expect(body).not.toContain('hidden-html-error');
      expect(body).not.toContain('Stack Trace');
    } finally {
      restoreEnv('PUTNAMI_DEV_ERRORS', previousVal);
    }
  });

  it('suppresses error details in production', async () => {
    const previousNodeEnv = process.env.NODE_ENV;
    process.env.NODE_ENV = 'production';
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('prod-error');
    });
    await start(p);

    try {
      const res = await fetch(`${baseUrl}/fail`);
      expect(res.status).toBe(500);
      const body = await res.json();
      expect(body.error).toBe('Internal Server Error');
      expect(body.stack).toBeUndefined();
    } finally {
      restoreEnv('NODE_ENV', previousNodeEnv);
    }
  });

  it('HTML page includes source context for errors with stack traces', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new Error('source-context-test');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
    const body = await res.text();
    expect(body).toContain('Source');
    expect(body).toContain('source-context-test');
    // Should contain vscode:// links for clickable file paths
    expect(body).toContain('vscode://file');
  });

  it('links a Windows stack frame to VS Code with forward slashes', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      const error = new Error('windows-frame-test');
      error.stack = 'Error: windows-frame-test\n    at handler (C:\\w\\app\\src\\api\\get.ts:3:17)';
      throw error;
    });
    await start(p);

    const body = await (await fetch(`${baseUrl}/fail`, HTML_ACCEPT)).text();
    expect(body).toContain('href="vscode://file/C:/w/app/src/api/get.ts:3:17"');
  });

  it('renders HttpException with correct status code', async () => {
    const p = http({ port: 0 });
    p.get('/fail', () => {
      throw new HttpException('Not Found', 404);
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
    expect(res.status).toBe(404);
    expect(res.headers.get('content-type')).toContain('text/html');
    const body = await res.text();
    expect(body).toContain('404');
    expect(body).toContain('Not Found');
  });

  it('includes request details in the error page', async () => {
    const p = http({ port: 0 });
    p.get('/users/[id]', () => {
      throw new Error('param-test');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/users/42`, HTML_ACCEPT);
    const body = await res.text();
    expect(body).toContain('GET');
    expect(body).toContain('/users/42');
  });

  it('user onError handler takes priority over dev page', async () => {
    const p = http({
      port: 0,
      onError: () => ({ custom: 'handled' }),
    });
    p.get('/fail', () => {
      throw new Error('custom-handled');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
    expect(res.status).toBe(500);
    const json = await res.json();
    expect(json.custom).toBe('handled');
  });

  it('falls through to dev page when onError returns undefined', async () => {
    const p = http({
      port: 0,
      onError: () => undefined,
    });
    p.get('/fail', () => {
      throw new Error('fallthrough-test');
    });
    await start(p);

    const res = await fetch(`${baseUrl}/fail`, HTML_ACCEPT);
    expect(res.headers.get('content-type')).toContain('text/html');
    const body = await res.text();
    expect(body).toContain('fallthrough-test');
  });
});
