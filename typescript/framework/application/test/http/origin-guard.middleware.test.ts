import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { application } from '../../src/application';

const crossSite = {
  feature: 'typescript/authentication-authorization',
  requirement: 'cross-site-request-forgery',
  check: 'a-cross-site-state-change-is-refused',
};
const trustedRequest = {
  feature: 'typescript/authentication-authorization',
  requirement: 'cross-site-request-forgery',
  check: 'a-trusted-or-non-browser-request-passes',
};
const reportMode = {
  feature: 'typescript/authentication-authorization',
  requirement: 'cross-site-request-forgery',
  check: 'report-mode-and-a-disabled-guard-let-requests-through',
};

describe('OriginGuardMiddleware', () => {
  describe('safe methods', () => {
    specTest('should allow GET with cross-origin Origin header', trustedRequest, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://evil.com' },
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    it('should allow OPTIONS with cross-origin Origin header', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        method: 'OPTIONS',
        headers: { Origin: 'https://evil.com' },
      });
      // OPTIONS is auto-handled (200 or 204)
      expect(res.status).toBeLessThan(400);

      await app.stop();
    });
  });

  describe('Sec-Fetch-Site', () => {
    it('should allow POST with Sec-Fetch-Site: same-origin', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { 'Sec-Fetch-Site': 'same-origin' },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    it('should allow POST with Sec-Fetch-Site: none', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { 'Sec-Fetch-Site': 'none' },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    it('should allow POST with Sec-Fetch-Site: same-site by default', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { 'Sec-Fetch-Site': 'same-site' },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    specTest('should reject POST with Sec-Fetch-Site: same-site when allowSameSite is false', crossSite, async () => {
      const httpPlugin = http({ port: 0, originGuard: { allowSameSite: false } });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { 'Sec-Fetch-Site': 'same-site' },
        body: '{}',
      });
      expect(res.status).toBe(403);
      const body = await res.json();
      expect(body.error).toBe('Cross-origin request blocked');

      await app.stop();
    });

    specTest('should reject POST with Sec-Fetch-Site: cross-site', crossSite, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: {
          'Sec-Fetch-Site': 'cross-site',
          Origin: 'https://evil.com',
        },
        body: '{}',
      });
      expect(res.status).toBe(403);

      await app.stop();
    });

    specTest('should allow cross-site POST when origin is trusted', trustedRequest, async () => {
      const httpPlugin = http({
        port: 0,
        originGuard: { trustedOrigins: ['https://admin.example.com'] },
      });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: {
          'Sec-Fetch-Site': 'cross-site',
          Origin: 'https://admin.example.com',
        },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });
  });

  describe('Origin fallback', () => {
    it('should allow POST when Origin matches Host', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const port = httpPlugin.getServer()?.port;
      const baseUrl = `http://localhost:${port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { Origin: `http://localhost:${port}` },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    specTest('should reject POST when Origin does not match Host', crossSite, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { Origin: 'https://evil.com' },
        body: '{}',
      });
      expect(res.status).toBe(403);

      await app.stop();
    });

    specTest('should allow POST with no Origin header (non-browser client)', trustedRequest, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    specTest('should reject PUT with mismatched Origin', crossSite, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.put('/item', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/item`, {
        method: 'PUT',
        headers: { Origin: 'https://evil.com' },
        body: '{}',
      });
      expect(res.status).toBe(403);

      await app.stop();
    });

    specTest('should reject DELETE with mismatched Origin', crossSite, async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.delete('/item', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/item`, {
        method: 'DELETE',
        headers: { Origin: 'https://evil.com' },
      });
      expect(res.status).toBe(403);

      await app.stop();
    });
  });

  describe('configuration', () => {
    specTest('should be disabled when originGuard is false', reportMode, async () => {
      const httpPlugin = http({ port: 0, originGuard: false });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { Origin: 'https://evil.com' },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    specTest('should allow through in report mode', reportMode, async () => {
      const httpPlugin = http({ port: 0, originGuard: { mode: 'report' } });
      httpPlugin.post('/submit', () => HttpResponse.json({ ok: true }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: { Origin: 'https://evil.com' },
        body: '{}',
      });
      expect(res.status).toBe(200);

      await app.stop();
    });
  });
});
