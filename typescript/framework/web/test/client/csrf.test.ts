import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { getCsrfToken } from '../../src/client/form/csrf';

describe('CSRF cookie parsing', () => {
  afterEach(() => {
    Reflect.deleteProperty(globalThis, 'document');
  });

  const withCookies = (cookie: string) => {
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie },
    });
  };

  it('returns undefined when document is not available', () => {
    expect(getCsrfToken()).toBeUndefined();
  });

  specTest(
    'parses a simple CSRF token',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'the-csrf-cookie-token-is-read-by-the-client',
    },
    () => {
      withCookies('_csrf=abc123');
      expect(getCsrfToken()).toBe('abc123');
    },
  );

  it('parses CSRF token among multiple cookies', () => {
    withCookies('session=xyz; _csrf=token456; theme=dark');
    expect(getCsrfToken()).toBe('token456');
  });

  it('returns undefined when CSRF cookie is not present', () => {
    withCookies('session=xyz; theme=dark');
    expect(getCsrfToken()).toBeUndefined();
  });

  it('handles values containing equals signs', () => {
    withCookies('_csrf=dG9rZW4=value==');
    expect(getCsrfToken()).toBe('dG9rZW4=value==');
  });

  it('handles empty value', () => {
    withCookies('_csrf=');
    expect(getCsrfToken()).toBe('');
  });

  it('handles empty cookie string', () => {
    withCookies('');
    expect(getCsrfToken()).toBeUndefined();
  });
});
