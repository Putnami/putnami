import { describe, expect, it } from 'bun:test';
import { http } from '../../src/http/http.plugin';
import type { HttpRequestContext, Server } from '../../src/http/http-context.type';
import { HttpResponse } from '../../src/http/http-response';
import { buildTrustedProxyMatcher } from '../../src/http/ip-prefix';
import type { RateLimitStore, RateLimitStoreEntry } from '../../src/http/rate-limit.middleware';
import { RateLimitMiddleware, trustedProxyKeyGenerator } from '../../src/http/rate-limit.middleware';
import { application } from '../../src/application';

describe('RateLimitMiddleware', () => {
  it('should allow requests within the limit', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(RateLimitMiddleware({ max: 5, windowMs: 60_000 }));
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(200);
    expect(res.headers.get('RateLimit-Limit')).toBe('5');
    expect(res.headers.get('RateLimit-Remaining')).toBe('4');
    expect(res.headers.get('RateLimit-Reset')).toBeTruthy();

    await app.stop();
  });

  it('should block requests exceeding the limit', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(RateLimitMiddleware({ max: 3, windowMs: 60_000 }));
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // Make 3 allowed requests
    for (let i = 0; i < 3; i++) {
      const res = await fetch(`${baseUrl}/data`);
      expect(res.status).toBe(200);
    }

    // 4th request should be blocked
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(429);
    const body = await res.json();
    expect(body.error).toBe('Too Many Requests');
    expect(res.headers.get('Retry-After')).toBeTruthy();
    expect(res.headers.get('RateLimit-Remaining')).toBe('0');

    await app.stop();
  });

  it('should use custom key generator', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(
      RateLimitMiddleware({
        max: 2,
        windowMs: 60_000,
        keyGenerator: (ctx) => ctx.headers.get('X-API-Key') || 'anonymous',
      }),
    );
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // user-a: 2 requests
    for (let i = 0; i < 2; i++) {
      const res = await fetch(`${baseUrl}/data`, {
        headers: { 'X-API-Key': 'user-a' },
      });
      expect(res.status).toBe(200);
    }

    // user-a: 3rd blocked
    const blockedA = await fetch(`${baseUrl}/data`, {
      headers: { 'X-API-Key': 'user-a' },
    });
    expect(blockedA.status).toBe(429);

    // user-b: still allowed
    const allowedB = await fetch(`${baseUrl}/data`, {
      headers: { 'X-API-Key': 'user-b' },
    });
    expect(allowedB.status).toBe(200);

    await app.stop();
  });

  it('should support custom error message', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(
      RateLimitMiddleware({
        max: 1,
        windowMs: 60_000,
        message: { error: 'Slow down!', code: 'RATE_LIMITED' },
      }),
    );
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    await fetch(`${baseUrl}/data`);
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(429);
    const body = await res.json();
    expect(body.code).toBe('RATE_LIMITED');

    await app.stop();
  });

  it('should omit rate limit headers when disabled', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(RateLimitMiddleware({ max: 5, headers: false }));
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`);
    expect(res.status).toBe(200);
    expect(res.headers.get('RateLimit-Limit')).toBeNull();

    await app.stop();
  });

  it('should support custom store', async () => {
    const entries = new Map<string, RateLimitStoreEntry>();
    const store = {
      consume: (key: string, limit: number, windowMs: number) => {
        const now = Date.now();
        let entry = entries.get(key);
        if (!entry || now >= entry.resetAt) entry = { tokens: limit, resetAt: now + windowMs };
        entry.tokens -= 1;
        entries.set(key, entry);
        return { allowed: entry.tokens >= 0, remaining: Math.max(0, entry.tokens), resetAt: entry.resetAt };
      },
    };

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(RateLimitMiddleware({ max: 1, windowMs: 60_000, store }));
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const first = await fetch(`${baseUrl}/data`);
    expect(first.status).toBe(200);

    const blocked = await fetch(`${baseUrl}/data`);
    expect(blocked.status).toBe(429);

    await app.stop();
  });

  describe('atomicity', () => {
    const fakeCtx = {
      req: new Request('http://localhost/data'),
      headers: new Headers(),
    } as unknown as HttpRequestContext;

    const runConcurrent = async (middleware: ReturnType<typeof RateLimitMiddleware>, count: number) => {
      const responses = await Promise.all(
        Array.from({ length: count }, () => middleware(fakeCtx, async () => HttpResponse.json({ ok: true }))),
      );
      return {
        allowed: responses.filter((r) => r?.status !== 429).length,
        blocked: responses.filter((r) => r?.status === 429).length,
      };
    };

    it('should not bypass the limit with an async store under concurrent requests', async () => {
      // Simulates a Redis/DB store: every operation awaits (network latency), entries are
      // deserialized copies, and `consume` is atomic server-side (latency happens BEFORE the
      // accounting, which runs as one synchronous critical section).
      const entries = new Map<string, RateLimitStoreEntry>();
      const latency = () => new Promise<void>((resolve) => setTimeout(resolve, 1 + Math.floor(Math.random() * 5)));
      const store: RateLimitStore = {
        async consume(key, limit, windowMs) {
          await latency();
          const now = Date.now();
          let entry = entries.get(key);
          if (!entry || now >= entry.resetAt) {
            entry = { tokens: limit, resetAt: now + windowMs };
            entries.set(key, entry);
          }
          entry.tokens -= 1;
          return { allowed: entry.tokens >= 0, remaining: Math.max(0, entry.tokens), resetAt: entry.resetAt };
        },
      };

      const middleware = RateLimitMiddleware({ max: 5, windowMs: 60_000, store, keyGenerator: () => 'client' });
      const { allowed, blocked } = await runConcurrent(middleware, 25);

      expect(allowed).toBe(5);
      expect(blocked).toBe(20);
    });

    it('should not bypass the limit with the default memory store under concurrent requests', async () => {
      const middleware = RateLimitMiddleware({ max: 3, windowMs: 60_000, keyGenerator: () => 'client' });
      const { allowed, blocked } = await runConcurrent(middleware, 20);

      expect(allowed).toBe(3);
      expect(blocked).toBe(17);
    });

    it('should keep per-key isolation with concurrent requests across keys', async () => {
      let i = 0;
      const middleware = RateLimitMiddleware({
        max: 2,
        windowMs: 60_000,
        keyGenerator: () => `client-${i++ % 2}`,
      });
      const { allowed, blocked } = await runConcurrent(middleware, 10);

      expect(allowed).toBe(4);
      expect(blocked).toBe(6);
    });
  });
});

describe('RateLimitMiddleware trusted-proxy keying (Go parity)', () => {
  // Mirrors go/framework/http/middleware_test.go's keyForRequest helper: drive
  // the key function with a chosen socket peer and X-Forwarded-For header. The
  // peer comes from ctx.server.requestIP().address — the non-spoofable socket
  // address — while XFF is the client-controlled, spoofable header.
  function keyFor(trusted: string[], peer: string | undefined, xff?: string): string {
    const generate = trustedProxyKeyGenerator(buildTrustedProxyMatcher(trusted));
    const headers = new Headers();
    if (xff !== undefined) headers.set('X-Forwarded-For', xff);
    const server = peer === undefined ? undefined : ({ requestIP: () => ({ address: peer }) } as unknown as Server);
    return generate({ req: new Request('http://localhost/'), headers, server });
  }

  it('ignores spoofed XFF from an untrusted peer (key is the peer)', () => {
    expect(keyFor(['10.0.0.1'], '192.168.1.9', '203.0.113.7')).toBe('192.168.1.9');
    expect(keyFor(['10.0.0.0/8'], '172.16.0.1', '198.51.100.23')).toBe('172.16.0.1');
  });

  it('honors XFF from a trusted exact peer (key is the first hop)', () => {
    expect(keyFor(['10.0.0.1'], '10.0.0.1', '203.0.113.7, 10.0.0.1')).toBe('203.0.113.7');
  });

  it('honors XFF from a trusted IPv4 CIDR peer', () => {
    expect(keyFor(['10.0.0.0/8'], '10.4.5.6', '198.51.100.23')).toBe('198.51.100.23');
  });

  it('honors XFF from a trusted IPv6 CIDR peer', () => {
    expect(keyFor(['2001:db8::/32'], '2001:db8::1', '198.51.100.5')).toBe('198.51.100.5');
  });

  it('honors XFF from an IPv4-mapped IPv6 peer inside an IPv4 CIDR (adversarial edge)', () => {
    expect(keyFor(['10.0.0.0/8'], '::ffff:10.4.5.6', '198.51.100.9')).toBe('198.51.100.9');
  });

  it('never trusts XFF when the trusted list is empty', () => {
    expect(keyFor([], '203.0.113.9', '1.2.3.4')).toBe('203.0.113.9');
  });

  it('handles malformed XFF chains without crashing, taking the trimmed first hop', () => {
    // Trailing comma / extra hops → first hop, trimmed.
    expect(keyFor(['10.0.0.1'], '10.0.0.1', '203.0.113.7,')).toBe('203.0.113.7');
    expect(keyFor(['10.0.0.1'], '10.0.0.1', '203.0.113.7, 10.0.0.1, 10.0.0.2')).toBe('203.0.113.7');
    // Empty header falls back to the peer (Go: xff == "" → RemoteAddr).
    expect(keyFor(['10.0.0.1'], '10.0.0.1', '')).toBe('10.0.0.1');
    // A whitespace-only header is normalized to '' by the Headers API before the
    // middleware sees it, so it falls back to the peer — a strictly safer outcome
    // than Go's raw '' key, and still no crash.
    expect(keyFor(['10.0.0.1'], '10.0.0.1', '   ')).toBe('10.0.0.1');
    // A leading comma keeps a non-empty header value; its first hop is empty,
    // matching Go's strings.Cut + TrimSpace on ", x".
    expect(keyFor(['10.0.0.1'], '10.0.0.1', ', 203.0.113.7')).toBe('');
  });

  it('uses the peer when it is unparseable or missing (never throws)', () => {
    expect(keyFor(['10.0.0.0/8'], 'garbage', '1.2.3.4')).toBe('garbage');
    expect(keyFor(['10.0.0.0/8'], undefined, '1.2.3.4')).toBe('unknown');
  });
});

describe('RateLimitMiddleware trusted-proxy selection', () => {
  async function runOnce(
    middleware: ReturnType<typeof RateLimitMiddleware>,
    peer: string,
    xff?: string,
  ): Promise<number | undefined> {
    const headers = new Headers();
    if (xff !== undefined) headers.set('X-Forwarded-For', xff);
    const ctx = {
      req: new Request('http://localhost/'),
      headers,
      server: { requestIP: () => ({ address: peer }) },
    } as unknown as HttpRequestContext;
    const res = await middleware(ctx, async () => HttpResponse.json({ ok: true }));
    return res?.status;
  }

  it('treats an empty trustedProxies list as peer-only (safe default)', async () => {
    const middleware = RateLimitMiddleware({ max: 1, windowMs: 60_000, trustedProxies: [] });
    // XFF is ignored; both requests share the peer key.
    expect(await runOnce(middleware, '203.0.113.9', '1.1.1.1')).toBe(200);
    expect(await runOnce(middleware, '203.0.113.9', '2.2.2.2')).toBe(429);
  });
});
