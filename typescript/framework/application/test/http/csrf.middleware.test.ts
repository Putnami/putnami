import { describe, expect, it } from 'bun:test';
import { CSRF_TOKEN_CONTEXT_KEY } from '../../src/http/csrf.middleware';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { application } from '../../src/application';
import { api } from '../../src/api/api.plugin';
import { endpoint } from '../../src/api/route';

describe('CsrfMiddleware', () => {
  it('should allow safe methods (GET) without a token', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(200);

    // Should set the CSRF cookie
    const setCookie = res.headers.get('Set-Cookie');
    expect(setCookie).toContain('_csrf=');
    expect(setCookie).toContain('SameSite=Strict');
    expect(setCookie).toContain('Secure');

    await app.stop();
  });

  it('should preserve an explicit secure: false override for local HTTP development', async () => {
    const httpPlugin = http({ port: 0, csrf: { secure: false } });
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(200);

    const setCookie = res.headers.get('Set-Cookie');
    expect(setCookie).toContain('_csrf=');
    expect(setCookie).not.toContain('Secure');

    await app.stop();
  });

  it('should reject POST without CSRF token', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ data: 'test' }),
    });
    expect(res.status).toBe(403);
    const body = await res.json();
    expect(body.error).toBe('CSRF token mismatch');

    await app.stop();
  });

  it('should accept POST with valid CSRF token', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First GET to obtain the CSRF cookie
    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    // POST with the token in header and cookie
    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: `_csrf=${csrfToken}`,
        'X-CSRF-Token': csrfToken,
      },
      body: JSON.stringify({ data: 'test' }),
    });
    expect(postRes.status).toBe(200);
    expect(postRes.headers.get('Set-Cookie')).toBeNull();
    const body = await postRes.json();
    expect(body.submitted).toBe(true);

    await app.stop();
  });

  it('should reuse a valid incoming token without rewriting the cookie on concurrent requests', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    const tokenResponse = (ctx: unknown) =>
      HttpResponse.json({ token: (ctx as Record<string, unknown>)[CSRF_TOKEN_CONTEXT_KEY] ?? null });
    httpPlugin.get('/form', tokenResponse);
    httpPlugin.get('/prefetch', tokenResponse);

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const first = await fetch(`${baseUrl}/form`);
    const token = first.headers.get('Set-Cookie')?.match(/_csrf=([^;]+)/)?.[1];
    expect(token).toBeTruthy();

    const [form, prefetch] = await Promise.all(
      ['/form', '/prefetch'].map((path) =>
        fetch(`${baseUrl}${path}`, {
          headers: { Cookie: `_csrf=${token}` },
        }),
      ),
    );

    for (const res of [form, prefetch]) {
      expect(res.status).toBe(200);
      expect(res.headers.get('Set-Cookie')).toBeNull();
      expect((await res.json()).token).toBe(token);
    }

    await app.stop();
  });

  it('should replace an invalid signed cookie on a safe request', async () => {
    const httpPlugin = http({ port: 0, csrf: { secret: 'super-secret' } });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/form`, {
      headers: { Cookie: '_csrf=forged.invalid' },
    });

    expect(res.status).toBe(200);
    const setCookie = res.headers.get('Set-Cookie');
    expect(setCookie).toContain('_csrf=');
    expect(setCookie).not.toContain('forged.invalid');

    await app.stop();
  });

  it('should reject POST with mismatched CSRF token', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: '_csrf=validtoken',
        'X-CSRF-Token': 'wrongtoken',
      },
      body: JSON.stringify({ data: 'test' }),
    });
    expect(res.status).toBe(403);

    await app.stop();
  });

  it('should support custom cookie and header names', async () => {
    const httpPlugin = http({ port: 0, csrf: { cookieName: 'my-csrf', headerName: 'X-My-Token' } });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/my-csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const token = tokenMatch?.[1];

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: `my-csrf=${token}`,
        'X-My-Token': token,
      },
      body: JSON.stringify({}),
    });
    expect(postRes.status).toBe(200);
    expect(postRes.headers.get('Set-Cookie')).toBeNull();

    await app.stop();
  });

  it('should accept signed cookie tokens', async () => {
    const httpPlugin = http({ port: 0, csrf: { secret: 'super-secret' } });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const signedToken = tokenMatch?.[1] || '';
    const rawToken = signedToken.split('.')[0];

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: `_csrf=${signedToken}`,
        'X-CSRF-Token': rawToken,
      },
      body: JSON.stringify({}),
    });
    expect(postRes.status).toBe(200);
    expect(postRes.headers.get('Set-Cookie')).toBeNull();

    await app.stop();
  });

  it('should skip CSRF validation for routes with csrfExempt option', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.post('/token', () => HttpResponse.json({ access_token: 'abc' }), { csrfExempt: true });
    httpPlugin.post('/introspect', () => HttpResponse.json({ active: true }), { csrfExempt: true });
    httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // csrfExempt routes should succeed without CSRF token
    const tokenRes = await fetch(`${baseUrl}/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ grant_type: 'authorization_code' }),
    });
    expect(tokenRes.status).toBe(200);
    expect((await tokenRes.json()).access_token).toBe('abc');

    // No CSRF cookie should be set on exempt routes
    expect(tokenRes.headers.get('Set-Cookie')).toBeNull();

    const introspectRes = await fetch(`${baseUrl}/introspect`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: 'abc' }),
    });
    expect(introspectRes.status).toBe(200);

    // Non-exempt route should still require CSRF token
    const submitRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    expect(submitRes.status).toBe(403);

    await app.stop();
  });

  it('should let the router return 404 for an unmatched unsafe request', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/known', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/definitely-not-a-route`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });

    expect(res.status).toBe(404);
    expect(res.headers.get('Set-Cookie')).toBeNull();

    await app.stop();
  });

  it('should automatically exempt api() routes from CSRF validation', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.post('/consent', () => HttpResponse.json({ ok: true }));

    const apiPlugin = api({ autoScan: false });
    apiPlugin.register('/token', { POST: endpoint(() => ({ access_token: 'abc' })) }, 'POST');
    apiPlugin.register('/revoke', { POST: endpoint(() => ({ revoked: true })) }, 'POST');

    const app = application().use(httpPlugin).use(apiPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // api() routes should bypass CSRF without any token
    const tokenRes = await fetch(`${baseUrl}/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ grant_type: 'authorization_code' }),
    });
    expect(tokenRes.status).toBe(200);

    const revokeRes = await fetch(`${baseUrl}/revoke`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    expect(revokeRes.status).toBe(200);

    // http() route should still require CSRF token
    const consentRes = await fetch(`${baseUrl}/consent`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    expect(consentRes.status).toBe(403);

    await app.stop();
  });

  it('enforces CSRF on api() routes when opted in with api({ csrf: true })', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));

    const apiPlugin = api({ autoScan: false, csrf: true });
    apiPlugin.register('/orders', { POST: endpoint(() => ({ created: true })) }, 'POST');

    const app = application().use(httpPlugin).use(apiPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // Without a token the opted-in api() route is now rejected.
    const noTokenRes = await fetch(`${baseUrl}/orders`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    expect(noTokenRes.status).toBe(403);

    // Obtain a token from a safe request, then the POST succeeds.
    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const csrfToken = setCookie.match(/_csrf=([^;]+)/)?.[1];
    expect(csrfToken).toBeTruthy();

    const okRes = await fetch(`${baseUrl}/orders`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: `_csrf=${csrfToken}`,
        'X-CSRF-Token': csrfToken as string,
      },
      body: JSON.stringify({}),
    });
    expect(okRes.status).toBe(200);

    await app.stop();
  });

  it('should accept POST with CSRF token in form body field', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First GET to obtain the CSRF cookie
    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    // POST with the token in form body (no header) — simulates plain HTML form submission
    const formBody = new URLSearchParams();
    formBody.set('_csrf', csrfToken!);
    formBody.set('name', 'test');

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(200);
    const body = await postRes.json();
    expect(body.submitted).toBe(true);

    await app.stop();
  });

  it('should accept POST with CSRF form token when Content-Type has a no-space charset param', async () => {
    // Regression: media-type parsing must not require '; ' before params, else
    // `application/x-www-form-urlencoded;charset=utf-8` is not recognised as a
    // form body and the token is never extracted.
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    const formBody = new URLSearchParams();
    formBody.set('_csrf', csrfToken!);
    formBody.set('name', 'test');

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded;charset=utf-8',
        Cookie: `_csrf=${csrfToken}`,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(200);
    const body = await postRes.json();
    expect(body.submitted).toBe(true);

    await app.stop();
  });

  it('should reject POST with wrong CSRF token in form body field', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First GET to obtain the CSRF cookie
    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    // POST with wrong token in form body
    const formBody = new URLSearchParams();
    formBody.set('_csrf', 'wrong-token');

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(403);

    await app.stop();
  });

  it('should prefer header token over form body field', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First GET to obtain the CSRF cookie
    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    // POST with valid header but wrong body — header should take precedence
    const formBody = new URLSearchParams();
    formBody.set('_csrf', 'wrong-token');

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
        'X-CSRF-Token': csrfToken,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(200);

    await app.stop();
  });

  it('should support custom fieldName for form body token', async () => {
    const httpPlugin = http({ port: 0, csrf: { fieldName: 'csrf_token' } });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    const csrfToken = tokenMatch?.[1];

    // POST with custom field name
    const formBody = new URLSearchParams();
    formBody.set('csrf_token', csrfToken!);

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(200);

    await app.stop();
  });

  it('should reject oversized form bodies with 413 before extracting the CSRF token', async () => {
    const httpPlugin = http({ port: 0, csrf: true, maxBodySizeBytes: 1024 });
    let handlerCalled = false;
    httpPlugin.post('/submit', () => {
      handlerCalled = true;
      return HttpResponse.json({ submitted: true });
    });

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // A cookie token (any value without a secret) reaches the form-token
    // extraction path when no header token is sent — the pre-fix code would
    // buffer the whole body via formData() here.
    const formBody = new URLSearchParams();
    formBody.set('_csrf', 'sometoken');
    formBody.set('payload', 'x'.repeat(4096));

    const res = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: '_csrf=sometoken',
      },
      body: formBody.toString(),
    });

    // Same behavior as parseBody's maxBodySizeBytes guard: 413 Payload Too
    // Large — not a 403 CSRF mismatch after buffering the oversized body.
    expect(res.status).toBe(413);
    expect(await res.text()).toContain('Payload Too Large');
    expect(handlerCalled).toBe(false);

    await app.stop();
  });

  it('should honour a CsrfOptions maxBodySizeBytes override for form-token extraction', async () => {
    // Server default limit (1 MiB) stays in place; the CSRF-level override is
    // stricter and must win for the form-token extraction path.
    const httpPlugin = http({ port: 0, csrf: { maxBodySizeBytes: 512 } });
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const formBody = new URLSearchParams();
    formBody.set('_csrf', 'sometoken');
    formBody.set('payload', 'x'.repeat(2048));

    const res = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: '_csrf=sometoken',
      },
      body: formBody.toString(),
    });
    expect(res.status).toBe(413);

    await app.stop();
  });

  it('should still accept form-body tokens within maxBodySizeBytes', async () => {
    const httpPlugin = http({ port: 0, csrf: true, maxBodySizeBytes: 1024 });
    httpPlugin.get('/form', () => HttpResponse.json({ ok: true }));
    httpPlugin.post('/submit', () => HttpResponse.json({ submitted: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    const getRes = await fetch(`${baseUrl}/form`);
    const setCookie = getRes.headers.get('Set-Cookie') || '';
    const csrfToken = setCookie.match(/_csrf=([^;]+)/)?.[1];
    expect(csrfToken).toBeTruthy();

    const formBody = new URLSearchParams();
    formBody.set('_csrf', csrfToken!);

    const postRes = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
      },
      body: formBody.toString(),
    });
    expect(postRes.status).toBe(200);
    expect((await postRes.json()).submitted).toBe(true);

    await app.stop();
  });

  it('should reject when using memory store without issued token', async () => {
    const httpPlugin = http({ port: 0, csrf: { storage: 'memory' } });
    httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/submit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: '_csrf=random',
        'X-CSRF-Token': 'random',
      },
      body: JSON.stringify({}),
    });
    expect(res.status).toBe(403);

    await app.stop();
  });

  it('publishes the effective token on the request context for SSR renderers', async () => {
    const httpPlugin = http({ port: 0, csrf: true });
    httpPlugin.get('/form', (ctx) =>
      HttpResponse.json({ token: (ctx as unknown as Record<string, unknown>)[CSRF_TOKEN_CONTEXT_KEY] ?? null }),
    );

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First visit: the request carries no cookie yet, but the published token
    // must be the one this response's Set-Cookie establishes — this is what
    // lets SSR render a working <CsrfInput /> on the very first response.
    const first = await fetch(`${baseUrl}/form`);
    const firstBody = await first.json();
    const setCookie = first.headers.get('Set-Cookie') || '';
    const tokenMatch = setCookie.match(/_csrf=([^;]+)/);
    expect(tokenMatch).toBeTruthy();
    expect(firstBody.token).toBe(tokenMatch?.[1]);

    // Return visit with the cookie: the published token is the cookie's token.
    const second = await fetch(`${baseUrl}/form`, { headers: { Cookie: `_csrf=${firstBody.token}` } });
    const secondBody = await second.json();
    expect(secondBody.token).toBe(firstBody.token);

    await app.stop();
  });
});
