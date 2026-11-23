import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { HttpResponse } from '../../src/http/http-response';
import { CSP_NONCE_CONTEXT_KEY, SecurityHeadersMiddleware } from '../../src/http/security-headers.middleware';

describe('SecurityHeadersMiddleware', () => {
  // Minimal context stub; `secured()` defaults to true (HTTPS) so HSTS is emitted.
  const ctx = (overrides: Record<string, unknown> = {}) => ({ secured: () => true, ...overrides }) as never;

  it('adds all default security headers', async () => {
    const middleware = SecurityHeadersMiddleware();
    const response = await middleware(ctx(), async () => new HttpResponse('ok'));

    expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
    expect(response?.getHeader('X-Frame-Options')).toBe('DENY');
    expect(response?.getHeader('Referrer-Policy')).toBe('strict-origin-when-cross-origin');
    expect(response?.getHeader('Strict-Transport-Security')).toBe('max-age=31536000; includeSubDomains');
    expect(response?.getHeader('X-XSS-Protection')).toBe('0');
    expect(response?.getHeader('Content-Security-Policy')).toContain("default-src 'self'");
    expect(response?.getHeader('Permissions-Policy')).toContain('camera=()');
  });

  specTest(
    'omits HSTS over a plaintext (insecure) request',
    {
      feature: 'typescript/http-services',
      requirement: 'secure-by-default',
      check: 'hsts-is-sent-only-over-https',
    },
    async () => {
      const middleware = SecurityHeadersMiddleware();
      const response = await middleware(ctx({ secured: () => false }), async () => new HttpResponse('ok'));

      expect(response?.getHeader('Strict-Transport-Security')).toBeUndefined();
      // The rest of the header suite is still applied.
      expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
      expect(response?.getHeader('Content-Security-Policy')).toContain("default-src 'self'");
    },
  );

  it('allows overriding specific headers', async () => {
    const middleware = SecurityHeadersMiddleware({
      frameOptions: 'SAMEORIGIN',
    });
    const response = await middleware(ctx(), async () => new HttpResponse('ok'));

    expect(response?.getHeader('X-Frame-Options')).toBe('SAMEORIGIN');
  });

  specTest(
    'does not overwrite headers already set by inner middleware',
    {
      feature: 'typescript/http-services',
      requirement: 'secure-by-default',
      check: 'a-header-the-handler-set-wins',
    },
    async () => {
      const middleware = SecurityHeadersMiddleware();
      const response = await middleware(
        ctx(),
        async () =>
          new HttpResponse('ok', {
            headers: {
              'Content-Security-Policy': "default-src 'self'; script-src 'self' https://cdn.example.com",
              'X-Frame-Options': 'SAMEORIGIN',
            },
          }),
      );

      expect(response?.getHeader('Content-Security-Policy')).toContain('https://cdn.example.com');
      expect(response?.getHeader('X-Frame-Options')).toBe('SAMEORIGIN');
      expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
    },
  );

  it('threads the request CSP nonce into the default policy', async () => {
    const middleware = SecurityHeadersMiddleware();
    const response = await middleware(ctx({ [CSP_NONCE_CONTEXT_KEY]: 'nonce-1' }), async () => new HttpResponse('ok'));

    expect(response?.getHeader('Content-Security-Policy')).toContain("script-src 'self' 'nonce-nonce-1'");
  });

  it('merges the request CSP nonce into an explicit policy override', async () => {
    const middleware = SecurityHeadersMiddleware({
      contentSecurityPolicy: "default-src 'self'; script-src 'self' https://cdn.example.com",
    });
    const response = await middleware(
      ctx({ [CSP_NONCE_CONTEXT_KEY]: 'nonce-1' }),
      async () =>
        new HttpResponse('ok', {
          headers: {
            'Content-Security-Policy': "default-src 'self'; script-src 'self' 'nonce-nonce-1'",
          },
        }),
    );

    expect(response?.getHeader('Content-Security-Policy')).toBe(
      "default-src 'self'; script-src 'self' https://cdn.example.com 'nonce-nonce-1'",
    );
  });

  it('allows disabling specific headers with false', async () => {
    const middleware = SecurityHeadersMiddleware({
      frameOptions: false,
      xssProtection: false,
    });
    const response = await middleware(ctx(), async () => new HttpResponse('ok'));

    expect(response?.getHeader('X-Frame-Options')).toBeUndefined();
    expect(response?.getHeader('X-XSS-Protection')).toBeUndefined();
    // Other headers should still be present
    expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
  });

  it('returns undefined when next returns undefined', async () => {
    const middleware = SecurityHeadersMiddleware();
    const response = await middleware(ctx(), async () => undefined);

    expect(response).toBeUndefined();
  });

  it('appends security headers alongside an existing Content-Type', async () => {
    const middleware = SecurityHeadersMiddleware();
    const response = await middleware(
      ctx(),
      async () => new HttpResponse('{}', { headers: { 'Content-Type': 'application/json' } }),
    );

    expect(response?.getHeader('Content-Type')).toBe('application/json');
    expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
    expect(response?.getHeader('Content-Security-Policy')).toBe("frame-ancestors 'none'");
  });

  specTest(
    'a JSON response carries only nosniff, frame-ancestors and HSTS',
    {
      feature: 'typescript/http-services',
      requirement: 'secure-by-default',
      check: 'a-json-response-carries-only-nosniff-and-frame-ancestors',
    },
    async () => {
      const middleware = SecurityHeadersMiddleware();
      for (const [contentType, secured] of [
        ['application/json', true],
        ['application/json', false],
        ['application/problem+json; charset=utf-8', true],
      ] as const) {
        const response = await middleware(ctx({ secured: () => secured }), async () =>
          contentType === 'application/json'
            ? HttpResponse.json({ ok: true })
            : new HttpResponse('{}', { headers: { 'Content-Type': contentType } }),
        );
        const expected: [string, string][] = [
          ['Content-Type', contentType],
          ['X-Content-Type-Options', 'nosniff'],
          ['Content-Security-Policy', "frame-ancestors 'none'"],
        ];
        if (secured) expected.push(['Strict-Transport-Security', 'max-age=31536000; includeSubDomains']);
        expect([...(response?.getHeaderEntries() ?? [])].sort()).toEqual(expected.sort());
        // The native response carries exactly the same headers.
        const native = response?.get();
        expect([...(native?.headers.entries() ?? [])].length).toBe(expected.length);
        expect(native?.headers.get('content-security-policy')).toBe("frame-ancestors 'none'");
      }
    },
  );

  it('keeps on a JSON response every header an option sets explicitly', async () => {
    const middleware = SecurityHeadersMiddleware({
      frameOptions: 'SAMEORIGIN',
      contentSecurityPolicy: "default-src 'none'",
      referrerPolicy: false,
    });
    const response = await middleware(ctx({ secured: () => false }), async () => HttpResponse.json({ ok: true }));

    expect(response?.getHeader('X-Frame-Options')).toBe('SAMEORIGIN');
    expect(response?.getHeader('Content-Security-Policy')).toBe("default-src 'none'");
    expect(response?.getHeader('Referrer-Policy')).toBeUndefined();
    expect(response?.getHeader('Permissions-Policy')).toBeUndefined();
    expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
  });

  it('copies the shared JSON header list before a later header change', async () => {
    const middleware = SecurityHeadersMiddleware();
    const first = await middleware(ctx({ secured: () => false }), async () => HttpResponse.json({ n: 1 }));
    first?.setHeader('X-Correlation-ID', 'abc');
    first?.pushHeader('Set-Cookie', 'a=1');

    const second = await middleware(ctx({ secured: () => false }), async () => HttpResponse.json({ n: 2 }));
    expect(second?.getHeader('X-Correlation-ID')).toBeUndefined();
    expect(second?.getHeaderEntries()).toHaveLength(3);

    const native = first?.get();
    expect(native?.headers.get('x-correlation-id')).toBe('abc');
    expect(native?.headers.get('set-cookie')).toBe('a=1');
    expect(native?.headers.get('x-content-type-options')).toBe('nosniff');
    expect(second?.get().headers.get('x-correlation-id')).toBeNull();
  });

  it('applies consistently across repeated requests on a shared instance', async () => {
    // The middleware precomputes its header set once at construction; ensure the
    // shared state is not mutated or leaked between requests.
    const middleware = SecurityHeadersMiddleware();
    const responses = await Promise.all([
      middleware(ctx(), async () => new HttpResponse('ok')),
      middleware(ctx(), async () => new HttpResponse('ok')),
      middleware(ctx(), async () => new HttpResponse('ok')),
    ]);

    for (const response of responses) {
      expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
      expect(response?.getHeader('Content-Security-Policy')).toContain("default-src 'self'");
      // Each request must yield exactly one of each header (no accumulation).
      const csp = response?.getHeaderEntries().filter(([n]) => n === 'Content-Security-Policy');
      expect(csp?.length).toBe(1);
    }
  });
});
