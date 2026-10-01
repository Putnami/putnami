import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { buildCspHeader, generateCspNonce } from '../../src/ssr/csp';

describe('generateCspNonce', () => {
  it('returns a base64-encoded string', () => {
    const nonce = generateCspNonce();
    expect(typeof nonce).toBe('string');
    expect(nonce.length).toBeGreaterThan(0);
    // 16 bytes → 24 base64 characters (with padding)
    expect(nonce.length).toBe(24);
  });

  specTest(
    'generates unique nonces on each call',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'a-per-request-nonce-is-generated-per-call',
    },
    () => {
      const a = generateCspNonce();
      const b = generateCspNonce();
      expect(a).not.toBe(b);
    },
  );

  it('produces valid base64', () => {
    const nonce = generateCspNonce();
    const decoded = Buffer.from(nonce, 'base64');
    expect(decoded.length).toBe(16);
  });
});

describe('buildCspHeader', () => {
  specTest(
    'returns sensible defaults when called with no options',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'the-default-policy-is-applied-when-no-options-are-given',
    },
    () => {
      const header = buildCspHeader();
      expect(header).toContain("default-src 'self'");
      expect(header).toContain("script-src 'self'");
      expect(header).toContain("style-src 'self' 'unsafe-inline'");
      expect(header).toContain("img-src 'self' data:");
      expect(header).toContain("font-src 'self'");
      expect(header).toContain("connect-src 'self'");
      expect(header).toContain("frame-ancestors 'none'");
      expect(header).toContain("base-uri 'self'");
      expect(header).toContain("form-action 'self'");
    },
  );

  specTest(
    'includes nonce in script-src when provided',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'the-nonce-reaches-the-script-src-directive',
    },
    () => {
      const header = buildCspHeader({ nonce: 'abc123' });
      expect(header).toContain("script-src 'self' 'nonce-abc123'");
    },
  );

  specTest(
    'does not include nonce directive when nonce is empty',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'an-empty-nonce-emits-no-nonce-directive',
    },
    () => {
      const header = buildCspHeader({});
      expect(header).not.toContain('nonce-');
    },
  );

  it('appends additional script-src values', () => {
    const header = buildCspHeader({ scriptSrc: ['https://cdn.example.com'] });
    expect(header).toContain("script-src 'self' https://cdn.example.com");
  });

  it('appends additional style-src values', () => {
    const header = buildCspHeader({ styleSrc: ['https://fonts.googleapis.com'] });
    expect(header).toContain("style-src 'self' 'unsafe-inline' https://fonts.googleapis.com");
  });

  it('appends additional connect-src values', () => {
    const header = buildCspHeader({ connectSrc: ['wss://api.example.com'] });
    expect(header).toContain("connect-src 'self' wss://api.example.com");
  });

  it('appends additional img-src values', () => {
    const header = buildCspHeader({ imgSrc: ['https://images.example.com'] });
    expect(header).toContain("img-src 'self' data: https://images.example.com");
  });

  it('appends additional font-src values', () => {
    const header = buildCspHeader({ fontSrc: ['https://fonts.gstatic.com'] });
    expect(header).toContain("font-src 'self' https://fonts.gstatic.com");
  });

  it('allows overriding directives via directives option', () => {
    const header = buildCspHeader({
      directives: {
        'frame-ancestors': "'self' https://parent.example.com",
        'report-uri': '/csp-violations',
      },
    });
    expect(header).toContain("frame-ancestors 'self' https://parent.example.com");
    expect(header).toContain('report-uri /csp-violations');
  });

  it('combines nonce with additional script sources', () => {
    const header = buildCspHeader({
      nonce: 'x',
      scriptSrc: ['https://cdn.example.com'],
    });
    expect(header).toContain("script-src 'self' 'nonce-x' https://cdn.example.com");
  });
});
