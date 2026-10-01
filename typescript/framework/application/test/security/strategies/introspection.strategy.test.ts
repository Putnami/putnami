import { afterEach, describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { resolveScopes } from '../../../src/security/security.utils';
import {
  type AuthStrategy,
  introspectCacheKey,
  introspectionStrategy,
  introspectRetryDelayMs,
  MAX_INTROSPECT_RETRY_DELAY_MS,
  MAX_INTROSPECT_RETRY_JITTER_MS,
  MemoryIntrospectionCache,
  MIN_INTROSPECT_RETRY_JITTER_MS,
} from '../../../src/security/strategies';

// TypeScript twin of go/framework/security/introspect_test.go and the
// through-the-middleware breaker cases in introspect_breaker_test.go. Global
// fetch is mocked (as the OAuth service tests do) so the suite is hermetic — no
// real sockets — while still exercising the real request-building, body-cap,
// caching, singleflight, and breaker code paths.

const ENDPOINT = 'https://auth.test/introspect';
const ISSUER = 'https://auth.test';

interface MockRequest {
  req: Request;
  form: URLSearchParams;
  path: string;
  signal?: AbortSignal | null;
}
interface MockResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}
type MockHandler = (c: MockRequest) => MockResult | Promise<MockResult>;

let restoreFetch: (() => void) | undefined;

function mockFetch(handler: MockHandler): { calls: () => number } {
  let calls = 0;
  const original = globalThis.fetch;
  restoreFetch = () => {
    globalThis.fetch = original;
  };
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls += 1;
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const bodyText = typeof init?.body === 'string' ? init.body : '';
    const result = await handler({
      req: new Request(url, init),
      form: new URLSearchParams(bodyText),
      path: new URL(url).pathname,
      signal: init?.signal,
    });
    return new Response(JSON.stringify(result.body), {
      status: result.status,
      headers: { 'Content-Type': 'application/json', ...result.headers },
    });
  }) as typeof fetch;
  return { calls: () => calls };
}

afterEach(() => {
  restoreFetch?.();
  restoreFetch = undefined;
});

function futureExp(seconds: number): number {
  return Math.floor(Date.now() / 1000) + seconds;
}

function activeJSON(sub: string): Record<string, unknown> {
  return { active: true, sub, exp: futureExp(3600) };
}

function basicAuth(req: Request): { user: string; pass: string } | undefined {
  const header = req.headers.get('Authorization');
  if (!header?.startsWith('Basic ')) return undefined;
  const decoded = Buffer.from(header.slice('Basic '.length), 'base64').toString('utf8');
  const idx = decoded.indexOf(':');
  return { user: decoded.slice(0, idx), pass: decoded.slice(idx + 1) };
}

async function runIntrospect(
  strategy: AuthStrategy,
  token: string,
): Promise<{ principal: Awaited<ReturnType<AuthStrategy>>; logger: MemoryLogger }> {
  const logger = new MemoryLogger();
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const ctx = { logger, req: new Request('http://localhost/', { headers }) } as unknown as HttpRequestContext;
  const principal = await runInContext(ctx, () => strategy(ctx));
  return { principal, logger };
}

function serialize(logger: MemoryLogger): string {
  return JSON.stringify(logger.entries);
}

function messages(logger: MemoryLogger): string[] {
  return logger.entries.map((entry) => entry.message);
}

/** A 429 Too Many Requests, with a Retry-After header unless `retryAfter` is undefined. */
function throttled(retryAfter?: string): MockResult {
  return { status: 429, body: {}, headers: retryAfter === undefined ? undefined : { 'Retry-After': retryAfter } };
}

const THROTTLED_LOG = 'token introspection throttled';
const SHORT_CIRCUITED_LOG = 'token introspection short-circuited: circuit breaker open';

// --- Resolver behavior ---

describe('introspectionStrategy — resolver behavior', () => {
  it('maps an active token to a principal (scopes, client_id, active flag dropped)', async () => {
    const srv = mockFetch(({ form }) => {
      expect(form.get('token')).toBe('opaque-1');
      return {
        status: 200,
        body: {
          active: true,
          sub: 'user-1',
          scope: 'read write',
          client_id: 'app-x',
          custom: 'val',
          exp: futureExp(3600),
        },
      };
    });
    const { principal } = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT }), 'opaque-1');
    expect(principal?.sub).toBe('user-1');
    expect(principal?.client_id).toBe('app-x');
    expect(resolveScopes(principal ?? {}, {})).toEqual(['read', 'write']);
    expect(principal?.['custom']).toBe('val');
    expect(principal?.['active']).toBeUndefined(); // RFC 7662 envelope flag must not leak
    // An introspected principal is a `user`: `.secure({ principalKind: 'user' })`
    // must be satisfiable via this strategy.
    expect(principal?.kind).toBe('user');
    expect(srv.calls()).toBe(1);
  });

  it('resolves nothing for an inactive token', async () => {
    mockFetch(() => ({ status: 200, body: { active: false } }));
    expect((await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT }), 'revoked')).principal).toBeUndefined();
  });

  it('never contacts the endpoint without a bearer token', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('x') }));
    expect((await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT }), '')).principal).toBeUndefined();
    expect(srv.calls()).toBe(0);
  });

  it('fails closed when the endpoint exceeds the timeout', async () => {
    mockFetch(async ({ signal }) => {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, 200);
        signal?.addEventListener('abort', () => {
          clearTimeout(timer);
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
      return { status: 200, body: activeJSON('slow') };
    });
    const { principal } = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, timeoutMs: 30 }), 'tok');
    expect(principal).toBeUndefined();
  });

  it('fails closed for a missing endpoint (no endpoint, no issuer)', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('x') }));
    expect((await runIntrospect(introspectionStrategy({}), 'tok')).principal).toBeUndefined();
    expect(srv.calls()).toBe(0);
  });

  it('rejects an insecure non-loopback endpoint without contacting it', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('x') }));
    expect(
      (await runIntrospect(introspectionStrategy({ endpoint: 'http://example.com/introspect' }), 'tok')).principal,
    ).toBeUndefined();
    expect(srv.calls()).toBe(0);
  });
});

// --- Client authentication ---

describe('introspectionStrategy — client authentication', () => {
  it('accepts correct client credentials via HTTP Basic and keeps them out of the body', async () => {
    mockFetch(({ req, form }) => {
      expect(form.has('client_id')).toBe(false);
      expect(form.has('client_secret')).toBe(false);
      const auth = basicAuth(req);
      if (auth?.user !== 'rs-id' || auth?.pass !== 'rs-secret') return { status: 401, body: {} };
      return { status: 200, body: { active: true, sub: 'basic-ok' } };
    });
    const { principal } = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'rs-id', clientSecret: 'rs-secret' }),
      'tok',
    );
    expect(principal?.sub).toBe('basic-ok');
  });

  it('fails closed when the endpoint rejects client credentials', async () => {
    mockFetch(({ req }) => {
      const auth = basicAuth(req);
      if (auth?.user !== 'rs-id' || auth?.pass !== 'rs-secret') return { status: 401, body: {} };
      return { status: 200, body: { active: true, sub: 'ok' } };
    });
    const { principal } = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'wrong', clientSecret: 'wrong' }),
      'tok',
    );
    expect(principal).toBeUndefined();
  });

  it('sends credentials in the form body for clientAuth post', async () => {
    mockFetch(({ req, form }) => {
      expect(basicAuth(req)).toBeUndefined();
      if (form.get('client_id') !== 'rs-id' || form.get('client_secret') !== 'rs-secret')
        return { status: 401, body: {} };
      return { status: 200, body: { active: true, sub: 'body-ok' } };
    });
    const { principal } = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'rs-id', clientSecret: 'rs-secret', clientAuth: 'post' }),
      'tok',
    );
    expect(principal?.sub).toBe('body-ok');
  });
});

// --- RFC 7523 client assertion ---

describe('introspectionStrategy — client assertion', () => {
  const ASSERTION_TYPE = 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer';

  it('authenticates via a client assertion and resolves it once per upstream call', async () => {
    const srv = mockFetch(({ form }) => {
      expect(form.get('client_assertion_type')).toBe(ASSERTION_TYPE);
      if (form.get('client_assertion') !== 'signed-jwt') return { status: 401, body: {} };
      return { status: 200, body: { active: true, sub: 'assertion-user', scope: 'read write', exp: futureExp(3600) } };
    });

    let resolverCalls = 0;
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      cache: new MemoryIntrospectionCache(),
      clientAssertion: () => {
        resolverCalls += 1;
        return 'signed-jwt';
      },
    });

    const first = await runIntrospect(strategy, 'tok');
    const second = await runIntrospect(strategy, 'tok');
    expect(first.principal?.sub).toBe('assertion-user');
    expect(second.principal?.sub).toBe('assertion-user');
    expect(srv.calls()).toBe(1); // second served from cache
    expect(resolverCalls).toBe(1); // resolved once per upstream call, not per request
  });

  it('takes precedence over clientId/clientSecret and never leaks them', async () => {
    for (const clientAuth of ['basic', 'post'] as const) {
      mockFetch(({ req, form }) => {
        expect(basicAuth(req)).toBeUndefined();
        expect(form.has('client_id')).toBe(false);
        expect(form.has('client_secret')).toBe(false);
        if (form.get('client_assertion') !== 'signed-jwt') return { status: 401, body: {} };
        return { status: 200, body: { active: true, sub: 'assertion-ok' } };
      });
      const { principal } = await runIntrospect(
        introspectionStrategy({
          endpoint: ENDPOINT,
          clientId: 'rs-id',
          clientSecret: 'rs-secret',
          clientAuth,
          clientAssertion: () => 'signed-jwt',
        }),
        'tok',
      );
      expect(principal?.sub).toBe('assertion-ok');
      restoreFetch?.();
    }
  });

  it('fails closed on a resolver error or empty assertion, never contacting the endpoint', async () => {
    for (const resolver of [
      () => {
        throw new Error('metadata server unreachable');
      },
      () => '',
    ]) {
      const srv = mockFetch(() => ({ status: 200, body: { active: true, sub: 'must-not-authenticate' } }));
      const { principal } = await runIntrospect(
        introspectionStrategy({
          endpoint: ENDPOINT,
          clientId: 'rs-id',
          clientSecret: 'rs-secret',
          clientAssertion: resolver,
        }),
        'tok',
      );
      expect(principal).toBeUndefined();
      expect(srv.calls()).toBe(0); // no secret-credential fallback
      restoreFetch?.();
    }
  });
});

// --- Caching ---

describe('introspectionStrategy — caching', () => {
  it('serves the second identical request from cache with independent principals', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('cached') }));
    const strategy = introspectionStrategy({ endpoint: ENDPOINT, cache: new MemoryIntrospectionCache() });
    const first = await runIntrospect(strategy, 'tok');
    const second = await runIntrospect(strategy, 'tok');
    expect(first.principal?.sub).toBe('cached');
    expect(second.principal?.sub).toBe('cached');
    expect(first.principal).not.toBe(second.principal); // independently-owned objects
    expect(srv.calls()).toBe(1);
  });

  it('scopes the cache by endpoint', async () => {
    const cache = new MemoryIntrospectionCache();
    const srv = mockFetch(({ req }) => ({
      status: 200,
      body: activeJSON(req.url.includes('auth-a') ? 'issuer-a' : 'issuer-b'),
    }));
    const a = await runIntrospect(
      introspectionStrategy({ endpoint: 'https://auth-a.test/introspect', cache }),
      'same-token',
    );
    const b = await runIntrospect(
      introspectionStrategy({ endpoint: 'https://auth-b.test/introspect', cache }),
      'same-token',
    );
    expect(a.principal?.sub).toBe('issuer-a');
    expect(b.principal?.sub).toBe('issuer-b');
    expect(srv.calls()).toBe(2);
  });

  it('scopes the cache by client credentials', async () => {
    const cache = new MemoryIntrospectionCache();
    const srv = mockFetch(({ req }) => {
      const auth = basicAuth(req);
      if (auth?.user === 'client-a' && auth?.pass === 'secret-a')
        return { status: 200, body: activeJSON('client-a-user') };
      if (auth?.user === 'client-b' && auth?.pass === 'secret-b')
        return { status: 200, body: activeJSON('client-b-user') };
      return { status: 401, body: {} };
    });
    const a = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'client-a', clientSecret: 'secret-a', cache }),
      'same-token',
    );
    const b = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'client-b', clientSecret: 'secret-b', cache }),
      'same-token',
    );
    expect(a.principal?.sub).toBe('client-a-user');
    expect(b.principal?.sub).toBe('client-b-user');
    expect(srv.calls()).toBe(2);
  });

  it('never caches an inactive result', async () => {
    const srv = mockFetch(() => ({ status: 200, body: { active: false } }));
    const strategy = introspectionStrategy({ endpoint: ENDPOINT, cache: new MemoryIntrospectionCache() });
    await runIntrospect(strategy, 'tok');
    await runIntrospect(strategy, 'tok');
    expect(srv.calls()).toBe(2);
  });

  it('hits the endpoint every request with no cache', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('x') }));
    const strategy = introspectionStrategy({ endpoint: ENDPOINT });
    await runIntrospect(strategy, 'tok');
    await runIntrospect(strategy, 'tok');
    expect(srv.calls()).toBe(2);
  });

  it('does not cache a result whose exp has already passed', async () => {
    const srv = mockFetch(() => ({ status: 200, body: { active: true, sub: 'x', exp: futureExp(-60) } }));
    const strategy = introspectionStrategy({ endpoint: ENDPOINT, cache: new MemoryIntrospectionCache() });
    const first = await runIntrospect(strategy, 'tok');
    expect(first.principal?.sub).toBe('x'); // active tokens authenticate even with a past exp
    await runIntrospect(strategy, 'tok');
    expect(srv.calls()).toBe(2);
  });

  it('never uses the raw token as a cache key (sha256-derived, secret-free)', async () => {
    mockFetch(() => ({ status: 200, body: activeJSON('x') }));
    const cache = new MemoryIntrospectionCache();
    await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, cache }), 'secret-token');
    expect(cache.get('secret-token')).toBeUndefined();
    expect(cache.get(introspectCacheKey(ENDPOINT, {}, 'secret-token'))).toBeDefined();
  });
});

// --- Audience ---

describe('introspectionStrategy — audience', () => {
  it('accepts a matching audience (string and array)', async () => {
    mockFetch(() => ({ status: 200, body: { active: true, sub: 'u', aud: 'my-svc' } }));
    expect(
      (await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, audience: 'my-svc' }), 'tok')).principal?.sub,
    ).toBe('u');
    restoreFetch?.();

    mockFetch(() => ({ status: 200, body: { active: true, sub: 'u', aud: ['a', 'my-svc', 'b'] } }));
    expect(
      (await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, audience: 'my-svc' }), 'tok')).principal,
    ).toBeDefined();
  });

  it('rejects a mismatched audience', async () => {
    mockFetch(() => ({ status: 200, body: { active: true, sub: 'u', aud: 'other-svc' } }));
    expect(
      (await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, audience: 'my-svc' }), 'tok')).principal,
    ).toBeUndefined();
  });

  it('re-checks the audience on a cache hit (audience is not part of the cache key)', async () => {
    const cache = new MemoryIntrospectionCache();
    const srv = mockFetch(() => ({
      status: 200,
      body: { active: true, sub: 'u', aud: 'svc-a', exp: futureExp(3600) },
    }));
    const a = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, audience: 'svc-a', cache }), 'tok');
    const b = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT, audience: 'svc-b', cache }), 'tok');
    expect(a.principal).toBeDefined();
    expect(b.principal).toBeUndefined(); // served A's cached payload, then rejected on audience
    expect(srv.calls()).toBe(1);
  });
});

// --- Discovery ---

describe('introspectionStrategy — discovery', () => {
  it('discovers the introspection endpoint from the issuer', async () => {
    mockFetch(({ path }) => {
      if (path === '/.well-known/openid-configuration')
        return { status: 200, body: { introspection_endpoint: ENDPOINT } };
      if (path === '/introspect') return { status: 200, body: { active: true, sub: 'discovered' } };
      return { status: 404, body: {} };
    });
    expect((await runIntrospect(introspectionStrategy({ issuer: ISSUER }), 'tok')).principal?.sub).toBe('discovered');
  });

  it('retries discovery after a transient failure, then memoizes success', async () => {
    let discoveryCalls = 0;
    mockFetch(({ path }) => {
      if (path.endsWith('openid-configuration')) {
        discoveryCalls += 1;
        if (discoveryCalls === 1) return { status: 503, body: {} };
        return { status: 200, body: { introspection_endpoint: ENDPOINT } };
      }
      return { status: 200, body: { active: true, sub: 'recovered' } };
    });
    const strategy = introspectionStrategy({ issuer: ISSUER });
    expect((await runIntrospect(strategy, 'tok')).principal).toBeUndefined(); // fails closed while discovery is down
    expect((await runIntrospect(strategy, 'tok')).principal?.sub).toBe('recovered');
    expect((await runIntrospect(strategy, 'tok-2')).principal?.sub).toBe('recovered'); // memoized
    expect(discoveryCalls).toBe(2);
  });
});

// --- Concurrency ---

describe('introspectionStrategy — concurrency', () => {
  it('collapses concurrent identical tokens into a single upstream call', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const srv = mockFetch(async () => {
      await gate;
      return { status: 200, body: activeJSON('sf') };
    });
    const strategy = introspectionStrategy({ endpoint: ENDPOINT }); // no cache: dedup must come from singleflight

    const runs = Array.from({ length: 20 }, () => runIntrospect(strategy, 'tok'));
    await Bun.sleep(50); // let the cohort enter the in-flight wait
    release();
    const settled = await Promise.all(runs);

    for (const { principal } of settled) {
      expect(principal?.sub).toBe('sf');
    }
    expect(srv.calls()).toBe(1);
  });
});

// --- Security invariants ---

describe('introspectionStrategy — security invariants', () => {
  it('caps the response body at 64 KiB before parsing (oversized valid JSON is truncated, not honored)', async () => {
    mockFetch(() => ({ status: 200, body: { active: true, sub: 'x', pad: 'a'.repeat(70 * 1024) } }));
    const { principal } = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT }), 'tok');
    // The full body is > 64 KiB; truncation makes it invalid JSON → treated as
    // inactive → no identity. Proves the cap runs before the parse.
    expect(principal).toBeUndefined();
  });

  it('accepts an active response comfortably under the 64 KiB cap', async () => {
    mockFetch(() => ({ status: 200, body: { active: true, sub: 'x', pad: 'a'.repeat(1024) } }));
    const { principal } = await runIntrospect(introspectionStrategy({ endpoint: ENDPOINT }), 'tok');
    expect(principal?.sub).toBe('x');
  });

  it('never logs the client secret', async () => {
    mockFetch(() => ({ status: 401, body: {} }));
    const { logger } = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientId: 'rs-id', clientSecret: 'super-secret-value' }),
      'QWERTY-BEARER-VALUE-123',
    );
    const logged = serialize(logger);
    expect(logged).not.toContain('super-secret-value');
    expect(logged).not.toContain('QWERTY-BEARER-VALUE-123'); // nor the bearer token
    expect(logger.entries.length).toBeGreaterThan(0); // a credentials-rejected error WAS logged
  });

  it('never logs the client assertion', async () => {
    mockFetch(() => ({ status: 401, body: {} }));
    const { logger } = await runIntrospect(
      introspectionStrategy({ endpoint: ENDPOINT, clientAssertion: () => 'top-secret-assertion' }),
      'tok',
    );
    expect(serialize(logger)).not.toContain('top-secret-assertion');
  });

  it('derives a cache key byte-identical to the Go twin (introspectCacheKey parity)', () => {
    const key = introspectCacheKey(
      'https://issuer.example.com/introspect',
      { issuer: 'https://issuer.example.com', clientId: 'client-a', clientSecret: 'secret-a' },
      'abc.def.ghi',
    );
    // Pinned against go/framework/security/introspect_test.go TestIntrospectCacheKey.
    expect(key).toBe('introspect:06bfb8c2a19753255e480ab283dae35cd8d34d4c14c9035190375df44c14625e');
  });
});

// --- Circuit breaker through the middleware ---

describe('introspectionStrategy — circuit breaker', () => {
  it('short-circuits uncached tokens after a sustained outage', async () => {
    const srv = mockFetch(() => ({ status: 503, body: {} })); // endpoint is out from the start
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      timeoutMs: 50,
      breaker: { failureThreshold: 2, openDurationMs: 3_600_000 },
    });

    // First two uncached tokens each reach the failing endpoint; the second opens it.
    await runIntrospect(strategy, 'a');
    await runIntrospect(strategy, 'b');
    expect(srv.calls()).toBe(2);

    for (let i = 0; i < 5; i++) {
      expect((await runIntrospect(strategy, `later${i}`)).principal).toBeUndefined();
    }
    expect(srv.calls()).toBe(2); // short-circuited: no more endpoint calls
  });

  it('keeps serving cached tokens while the breaker is open', async () => {
    let down = false;
    const srv = mockFetch(() => (down ? { status: 503, body: {} } : { status: 200, body: activeJSON('svc') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      cache: new MemoryIntrospectionCache(),
      timeoutMs: 50,
      breaker: { failureThreshold: 2, openDurationMs: 3_600_000 },
    });

    expect((await runIntrospect(strategy, 'X')).principal?.sub).toBe('svc'); // prime cache while healthy
    down = true;
    await runIntrospect(strategy, 'trip-0');
    await runIntrospect(strategy, 'trip-1'); // opens the breaker
    const callsAfterTrip = srv.calls();

    expect((await runIntrospect(strategy, 'X')).principal?.sub).toBe('svc'); // cached, no network
    expect((await runIntrospect(strategy, 'Y')).principal).toBeUndefined(); // uncached, fail closed
    expect(srv.calls()).toBe(callsAfterTrip); // no endpoint calls during the open window
  });

  it('never opens the breaker on a healthy endpoint', async () => {
    const srv = mockFetch(() => ({ status: 200, body: activeJSON('ok') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 2, openDurationMs: 3_600_000 },
    });
    for (let i = 0; i < 10; i++) {
      expect((await runIntrospect(strategy, `tok${i}`)).principal?.sub).toBe('ok');
    }
    expect(srv.calls()).toBe(10);
  });
});

// --- 429 backpressure: one bounded retry, collapsed through singleflight ---
//
// TypeScript twin of the Go throttling tests in introspect_test.go and
// introspect_breaker_test.go. Every behavioral test answers the 429 with
// `Retry-After: 0` (or a past date) so no test waits a real second.

describe('introspectionStrategy — 429 backpressure', () => {
  // Go: TestIntrospect_ThrottledThenOKRetriesOnceAndResolves.
  const retryAfterForms: [string, string | undefined][] = [
    ['delta-seconds zero', '0'],
    ['a past HTTP-date', new Date(Date.now() - 3_600_000).toUTCString()],
    ['an absent header (the jittered default)', undefined],
  ];
  for (const [form, retryAfter] of retryAfterForms) {
    it(`retries a 429 once after ${form} and authenticates the retried 200`, async () => {
      let seen = 0;
      const srv = mockFetch(({ form: body }) => {
        if (body.get('client_assertion') !== 'signed-jwt') return { status: 401, body: {} };
        seen += 1;
        return seen === 1 ? throttled(retryAfter) : { status: 200, body: activeJSON('after-throttle') };
      });
      let resolverCalls = 0;
      const strategy = introspectionStrategy({
        endpoint: ENDPOINT,
        cache: new MemoryIntrospectionCache(),
        breaker: { failureThreshold: 1, openDurationMs: 3_600_000 },
        clientAssertion: () => {
          resolverCalls += 1;
          return 'signed-jwt';
        },
      });

      const first = await runIntrospect(strategy, 'tok');
      expect(first.principal?.sub).toBe('after-throttle');
      expect(srv.calls()).toBe(2); // the 429 and one retry
      expect(resolverCalls).toBe(2); // the retry is a fresh upstream request
      expect(messages(first.logger)).not.toContain(THROTTLED_LOG);

      // The retried result is cached like any other active 200.
      expect((await runIntrospect(strategy, 'tok')).principal?.sub).toBe('after-throttle');
      // The breaker stayed closed (failureThreshold 1): an uncached token still
      // reaches the endpoint.
      const other = await runIntrospect(strategy, 'other');
      expect(other.principal?.sub).toBe('after-throttle');
      expect(messages(other.logger)).not.toContain(SHORT_CIRCUITED_LOG);
      expect(srv.calls()).toBe(3); // cache hit, then one call for the uncached token
    });
  }

  // Go: TestIntrospect_ThrottleBurstNeverOpensBreaker — the issue's acceptance
  // check. At failureThreshold 1 a single counted failure would open the breaker.
  it('fails a burst of 429s closed without ever opening the breaker', async () => {
    let throttling = true;
    const srv = mockFetch(() => (throttling ? throttled('0') : { status: 200, body: activeJSON('recovered') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 1, openDurationMs: 3_600_000 },
    });

    // Distinct uncached tokens: every request is its own singleflight cohort and
    // folds its own outcome into the breaker.
    const burst = 12;
    const settled = await Promise.all(Array.from({ length: burst }, (_, i) => runIntrospect(strategy, `burst-${i}`)));
    for (const { principal, logger } of settled) {
      expect(principal).toBeUndefined(); // a 429 never authenticates
      expect(messages(logger)).toContain(THROTTLED_LOG);
      expect(messages(logger)).not.toContain(SHORT_CIRCUITED_LOG);
    }
    expect(srv.calls()).toBe(2 * burst); // a first call and one retry per request, none short-circuited

    // The breaker is still closed: once the endpoint stops throttling, an
    // uncached token reaches it and resolves.
    throttling = false;
    expect((await runIntrospect(strategy, 'after-burst')).principal?.sub).toBe('recovered');
    expect(srv.calls()).toBe(2 * burst + 1);
  });

  // The same property through one collapsed cohort: every admitted waiter folds
  // the shared 429 into the breaker, and none of them may count it.
  it('fails a collapsed cohort of 12 identical throttled tokens closed without opening the breaker', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => {
      release = r;
    });
    let throttling = true;
    let seen = 0;
    const srv = mockFetch(async () => {
      seen += 1;
      if (seen === 1) await gate; // hold the first call open while the cohort piles up behind it
      return throttling ? throttled('0') : { status: 200, body: activeJSON('recovered') };
    });
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 1, openDurationMs: 3_600_000 },
    });

    const runs = Array.from({ length: 12 }, () => runIntrospect(strategy, 'tok'));
    await Bun.sleep(50); // let the cohort enter the in-flight wait
    release();
    const settled = await Promise.all(runs);

    for (const { principal, logger } of settled) {
      expect(principal).toBeUndefined();
      expect(messages(logger)).toContain(THROTTLED_LOG);
    }
    expect(srv.calls()).toBe(2); // one first call + one retry shared by the cohort

    throttling = false;
    expect((await runIntrospect(strategy, 'after-cohort')).principal?.sub).toBe('recovered');
    expect(srv.calls()).toBe(3);
  });

  // Go: TestIntrospect_ThrottleRetryCollapsesConcurrent.
  it('shares one retry across 20 concurrent identical tokens', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => {
      release = r;
    });
    let seen = 0;
    const srv = mockFetch(async () => {
      seen += 1;
      if (seen === 1) {
        await gate; // hold the first call open while the cohort piles up behind it
        return throttled('0');
      }
      return { status: 200, body: activeJSON('sf-throttled') };
    });
    const strategy = introspectionStrategy({ endpoint: ENDPOINT }); // no cache: dedup must come from singleflight

    const runs = Array.from({ length: 20 }, () => runIntrospect(strategy, 'tok'));
    await Bun.sleep(50); // let the cohort enter the in-flight wait
    release();
    const settled = await Promise.all(runs);

    for (const { principal } of settled) {
      expect(principal?.sub).toBe('sf-throttled');
    }
    expect(srv.calls()).toBe(2); // one first call + one retry shared by the cohort
  });

  // Go: TestIntrospect_ThrottledRetryServerErrorOpensBreaker.
  it('counts a 503 on the retry toward the breaker like any other 5xx', async () => {
    let seen = 0;
    const srv = mockFetch(() => {
      seen += 1;
      return seen === 1 ? throttled('0') : { status: 503, body: {} };
    });
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 1, openDurationMs: 3_600_000 },
    });

    const a = await runIntrospect(strategy, 'a');
    expect(a.principal).toBeUndefined();
    expect(srv.calls()).toBe(2); // first call + retry
    expect(messages(a.logger)).toContain('token introspection endpoint returned an unexpected status');
    expect(messages(a.logger)).not.toContain(THROTTLED_LOG);

    // The 503 opened the breaker (threshold 1): the next uncached token is
    // short-circuited without an upstream call.
    const b = await runIntrospect(strategy, 'b');
    expect(messages(b.logger)).toContain(SHORT_CIRCUITED_LOG);
    expect(srv.calls()).toBe(2);
  });

  // Go: TestIntrospect_ThrottleNeitherResetsNorExtendsFailureRun. Had the 429
  // counted, the breaker would open one request early; had it reset the run,
  // the second 503 would not open it.
  it('neither resets nor extends a failure run with a 429', async () => {
    let status = 503;
    const srv = mockFetch(() => (status === 429 ? throttled('0') : { status, body: activeJSON('svc') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 2, openDurationMs: 3_600_000 },
    });

    await runIntrospect(strategy, 'a'); // 503: failure 1 of 2
    status = 429;
    await runIntrospect(strategy, 'b'); // 429 twice: neutral
    status = 503;
    await runIntrospect(strategy, 'c'); // 503: failure 2 of 2, opens
    expect(srv.calls()).toBe(4); // the 429 did not open the breaker early

    status = 200;
    const d = await runIntrospect(strategy, 'd');
    expect(d.principal).toBeUndefined(); // the run survived the 429, so the second 503 opened it
    expect(messages(d.logger)).toContain(SHORT_CIRCUITED_LOG);
    expect(srv.calls()).toBe(4);
  });

  // Go: TestIntrospect_ThrottledTrialDoesNotWedgeBreaker. The white-box twin in
  // introspect-breaker.test.ts drives the same transition on an injected clock;
  // here a short real open window keeps the strategy's own breaker.
  it('frees the half-open probe slot when the trial is throttled', async () => {
    const openDurationMs = 30;
    let status = 503;
    const srv = mockFetch(() => (status === 429 ? throttled('0') : { status, body: activeJSON('svc') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      breaker: { failureThreshold: 1, openDurationMs },
    });

    await runIntrospect(strategy, 'a'); // 503: opens
    await Bun.sleep(2 * openDurationMs);

    status = 429;
    const trial = await runIntrospect(strategy, 'trial');
    expect(trial.principal).toBeUndefined();
    expect(messages(trial.logger)).toContain(THROTTLED_LOG); // the trial reached the endpoint

    status = 200;
    const next = await runIntrospect(strategy, 'next');
    expect(next.principal?.sub).toBe('svc'); // a stranded slot would deny this caller forever
    expect(srv.calls()).toBe(4); // 503, trial 429 + retry, recovery probe
  });

  // Go: TestIntrospect_ThrottleRetrySkippedWhenBudgetCannotCoverWait.
  it('skips the retry when the leader budget cannot cover the Retry-After wait', async () => {
    let throttling = true;
    const srv = mockFetch(() => (throttling ? throttled('1') : { status: 200, body: activeJSON('svc') }));
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      timeoutMs: 500, // clientAssertionTimeoutMs defaults to this: a 500ms budget for a 1s wait
      breaker: { failureThreshold: 1, openDurationMs: 3_600_000 },
    });

    const { principal, logger } = await runIntrospect(strategy, 'tok');
    expect(principal).toBeUndefined();
    expect(srv.calls()).toBe(1); // the retry was skipped
    expect(messages(logger)).toContain(THROTTLED_LOG); // not an endpoint failure
    expect(messages(logger)).not.toContain('token introspection request failed');

    // The skipped retry left the breaker closed (failureThreshold 1).
    throttling = false;
    expect((await runIntrospect(strategy, 'other')).principal?.sub).toBe('svc');
  });

  // The retry runs inside the leader budget, not on a fresh per-request timeout,
  // so the whole upstream operation stays bounded by clientAssertionTimeoutMs.
  it('bounds the retry by the leader budget left, not by a fresh request timeout', async () => {
    let seen = 0;
    mockFetch(async ({ signal }) => {
      seen += 1;
      if (seen === 1) return throttled('0');
      // Hang the retry; only an abort ends it before the failsafe.
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, 4000);
        signal?.addEventListener('abort', () => {
          clearTimeout(timer);
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
      return { status: 200, body: activeJSON('must-not-authenticate') };
    });
    const strategy = introspectionStrategy({
      endpoint: ENDPOINT,
      timeoutMs: 5000, // a fresh per-request timeout would outlast the failsafe
      clientAssertionTimeoutMs: 200, // the leader budget
    });

    const started = Date.now();
    const { principal, logger } = await runIntrospect(strategy, 'tok');
    expect(principal).toBeUndefined();
    expect(messages(logger)).toContain('token introspection request failed');
    expect(Date.now() - started).toBeLessThan(2500);
  });
});

// --- Retry-After parsing (Go: TestIntrospectRetryDelay, case for case) ---

describe('introspectRetryDelayMs', () => {
  // A sub-second now, so an HTTP-date (one-second resolution) just ahead of it
  // yields a delay strictly inside the cap: 2026-09-11T12:00:00.400Z.
  const now = Date.UTC(2026, 8, 11, 12, 0, 0, 400);
  const imfFixdate = (at: number): string => new Date(at).toUTCString();

  // Each case may carry its own now; the default is the one above.
  const cases: [string, string, number, number?][] = [
    ['delta-seconds zero', '0', 0],
    ['delta-seconds leading zeros', '00', 0],
    ['delta-seconds one', '1', 1000],
    ['delta-seconds with surrounding space', ' 1 ', 1000],
    ['delta-seconds oversized is capped', '3600', MAX_INTROSPECT_RETRY_DELAY_MS],
    ['delta-seconds beyond uint64 is capped', '99999999999999999999999', MAX_INTROSPECT_RETRY_DELAY_MS],
    ['HTTP-date inside the cap', imfFixdate(now + 600), 600],
    ['HTTP-date in obsolete RFC 850 form', 'Friday, 11-Sep-26 12:00:01 GMT', 600],
    ['HTTP-date beyond the cap is capped', imfFixdate(now + 3_600_000), MAX_INTROSPECT_RETRY_DELAY_MS],
    ['HTTP-date in the past retries at once', imfFixdate(now - 3_600_000), 0],
    // Beyond the Go table: the forms Go's http.ParseTime also accepts, pinned so
    // the two runtimes cannot drift. asctime carries no zone and is UTC in Go.
    ['HTTP-date in obsolete RFC 850 form with the UTC zone', 'Friday, 11-Sep-26 12:00:01 UTC', 600],
    ['HTTP-date in obsolete asctime form (UTC)', 'Fri Sep 11 12:00:01 2026', 600],
    [
      'HTTP-date in asctime form with a space-padded day',
      'Tue Sep  1 12:00:01 2026',
      600,
      Date.UTC(2026, 8, 1, 12, 0, 0, 400),
    ],
    ['HTTP-date with lowercase day and month names', 'fri, 11 sep 2026 12:00:01 GMT', 600],
  ];
  for (const [name, retryAfter, want, at] of cases) {
    it(name, () => {
      expect(introspectRetryDelayMs(retryAfter, at ?? now)).toBe(want);
    });
  }

  // Absent or malformed headers fall back to the jittered default, never to an
  // immediate retry and never beyond the cap.
  const malformed: Array<string | null | undefined> = [
    '',
    '   ',
    'soon',
    '-5',
    '+5',
    '1.5',
    '0x10',
    null, // Headers.get() when the header is absent
    undefined,
    // Dates Go's http.ParseTime rejects too.
    'Fri, 31 Feb 2026 12:00:01 GMT', // day out of range
    'Fri, 11 Sep 2026 24:00:00 GMT', // hour out of range
    'Fri, 11 Sep 2026 12:00:01 gmt', // the IMF-fixdate zone is a literal "GMT"
    'Fri, 1 Sep 2026 12:00:01 GMT', // IMF-fixdate needs a two-digit day
    // A deliberate divergence: Go resolves a non-GMT RFC 850 zone against the
    // process's local zone; TypeScript accepts only GMT and UTC there.
    'Friday, 11-Sep-26 12:00:01 PST',
  ];
  for (const retryAfter of malformed) {
    it(`uses the jittered default for ${JSON.stringify(retryAfter)}`, () => {
      for (let i = 0; i < 100; i++) {
        const got = introspectRetryDelayMs(retryAfter, now);
        expect(got).toBeGreaterThanOrEqual(MIN_INTROSPECT_RETRY_JITTER_MS);
        expect(got).toBeLessThan(MAX_INTROSPECT_RETRY_JITTER_MS);
      }
    });
  }

  it('keeps the jitter inside the retry cap', () => {
    expect(MAX_INTROSPECT_RETRY_JITTER_MS).toBeLessThanOrEqual(MAX_INTROSPECT_RETRY_DELAY_MS);
  });
});
