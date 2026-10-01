import { describe, expect, it } from 'bun:test';
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

  it('omits HSTS over a plaintext (insecure) request', async () => {
    const middleware = SecurityHeadersMiddleware();
    const response = await middleware(ctx({ secured: () => false }), async () => new HttpResponse('ok'));

    expect(response?.getHeader('Strict-Transport-Security')).toBeUndefined();
    // The rest of the header suite is still applied.
    expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
    expect(response?.getHeader('Content-Security-Policy')).toContain("default-src 'self'");
  });

  it('allows overriding specific headers', async () => {
    const middleware = SecurityHeadersMiddleware({
      frameOptions: 'SAMEORIGIN',
    });
    const response = await middleware(ctx(), async () => new HttpResponse('ok'));

    expect(response?.getHeader('X-Frame-Options')).toBe('SAMEORIGIN');
  });

  it('does not overwrite headers already set by inner middleware', async () => {
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
  });

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
    expect(response?.getHeader('X-Frame-Options')).toBe('DENY');
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
