import { describe, expect, it } from 'bun:test';
import { endpoint } from '../../src/api/route/endpoint';
import type { EndpointDefinition } from '../../src/api/route/endpoint.types';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';

/** Cross-site POST headers that the origin guard blocks by default. */
const CROSS_SITE = { Origin: 'https://evil.com', 'Sec-Fetch-Site': 'cross-site' } as const;

describe('minimal / secure:false fast lane', () => {
  describe('app-wide secure: false', () => {
    it('omits security headers and skips the origin guard', async () => {
      const plugin = http({ port: 0, secure: false });
      plugin.post('/echo', () => HttpResponse.json({ ok: true }));
      const app = application().use(plugin);
      await app.start();
      const base = `http://localhost:${plugin.getServer()?.port}`;

      const res = await fetch(`${base}/echo`, { method: 'POST', headers: CROSS_SITE });
      // Origin guard disabled → cross-origin POST is allowed.
      expect(res.status).toBe(200);
      // Security headers disabled.
      expect(res.headers.get('X-Frame-Options')).toBeNull();
      expect(res.headers.get('X-Content-Type-Options')).toBeNull();
      await res.arrayBuffer();

      await app.stop();
    });

    it('still honours an explicit securityHeaders override under secure:false', async () => {
      const plugin = http({ port: 0, secure: false, securityHeaders: { frameOptions: 'SAMEORIGIN' } });
      plugin.get('/x', () => HttpResponse.json({ ok: true }));
      const app = application().use(plugin);
      await app.start();
      const base = `http://localhost:${plugin.getServer()?.port}`;

      const res = await fetch(`${base}/x`);
      // Explicit per-option setting wins over the secure:false default.
      expect(res.headers.get('X-Frame-Options')).toBe('SAMEORIGIN');
      await res.arrayBuffer();

      await app.stop();
    });
  });

  describe('per-route minimal (security on app-wide)', () => {
    it('skips the security middleware for the minimal route only', async () => {
      const plugin = http({ port: 0 }); // secure by default
      plugin.post('/secured', () => HttpResponse.json({ ok: true }));
      plugin.post('/fast', () => HttpResponse.json({ ok: true }), { minimal: true });
      const app = application().use(plugin);
      await app.start();
      const base = `http://localhost:${plugin.getServer()?.port}`;

      // Normal route: cross-origin POST is blocked by the origin guard.
      const blocked = await fetch(`${base}/secured`, { method: 'POST', headers: CROSS_SITE });
      expect(blocked.status).toBe(403);
      await blocked.arrayBuffer();

      // Minimal route: origin guard skipped (200) and no security headers.
      const fast = await fetch(`${base}/fast`, { method: 'POST', headers: CROSS_SITE });
      expect(fast.status).toBe(200);
      expect(fast.headers.get('X-Frame-Options')).toBeNull();
      await fast.arrayBuffer();

      await app.stop();
    });

    it('does not leak the minimal opt-out across route fallthrough', async () => {
      const plugin = http({ port: 0 }); // secure by default
      // Two GET handlers at the same path: a minimal one that declines (returns
      // undefined), then a normal one. The fallthrough handler must still be secured.
      plugin.get('/x', () => undefined, { minimal: true });
      plugin.get('/x', () => HttpResponse.json({ ok: true }));
      const app = application().use(plugin);
      await app.start();
      const base = `http://localhost:${plugin.getServer()?.port}`;

      const res = await fetch(`${base}/x`);
      expect(res.status).toBe(200);
      expect(await res.json()).toEqual({ ok: true });
      // Because not every matched candidate is minimal, security headers apply.
      expect(res.headers.get('X-Frame-Options')).toBe('DENY');

      await app.stop();
    });

    it('keeps the security headers on normal routes', async () => {
      const plugin = http({ port: 0 });
      plugin.get('/normal', () => HttpResponse.json({ ok: true }));
      const app = application().use(plugin);
      await app.start();
      const base = `http://localhost:${plugin.getServer()?.port}`;

      const res = await fetch(`${base}/normal`);
      expect(res.headers.get('X-Frame-Options')).toBe('DENY');
      await res.arrayBuffer();

      await app.stop();
    });
  });

  describe('endpoint().minimal()', () => {
    it('marks the endpoint definition minimal', () => {
      const def = endpoint()
        .minimal()
        .handle(() => ({ ok: true })) as EndpointDefinition;
      expect(def.minimal).toBe(true);
    });

    it('is absent by default', () => {
      const def = endpoint().handle(() => ({ ok: true })) as EndpointDefinition;
      expect(def.minimal).toBeUndefined();
    });
  });
});
