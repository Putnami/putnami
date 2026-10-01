import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { HttpTransport } from '../../src/runtime/http-transport';
import { assertHttpUrl } from '../../src/runtime/url';
import { WebSocketTransport } from '../../src/runtime/ws-transport';

class UrlTestClient extends BaseClient {
  readonly serviceName = 'url-test';
}

describe('assertHttpUrl (SSRF / scheme allow-listing)', () => {
  specTest(
    'accepts http and https URLs',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'only-http-and-https-base-urls-are-accepted',
    },
    () => {
      expect(assertHttpUrl('http://example.com')).toBe('http://example.com');
      expect(assertHttpUrl('https://example.com:8443/api')).toBe('https://example.com:8443/api');
    },
  );

  specTest(
    'rejects file:// scheme',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'a-file-scheme-base-url-is-rejected',
    },
    () => {
      expect(() => assertHttpUrl('file:///etc/passwd')).toThrow(/scheme/);
    },
  );

  specTest(
    'rejects ftp:// scheme',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'an-ftp-scheme-base-url-is-rejected',
    },
    () => {
      expect(() => assertHttpUrl('ftp://example.com/x')).toThrow(/scheme/);
    },
  );

  specTest(
    'rejects malformed URLs',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'a-malformed-base-url-is-rejected',
    },
    () => {
      expect(() => assertHttpUrl('not a url')).toThrow(/not a valid URL/);
    },
  );

  test('includes the source label in the error', () => {
    expect(() => assertHttpUrl('file:///x', 'CustomSource')).toThrow(/CustomSource/);
  });
});

describe('transport constructors reject non-http baseUrl', () => {
  specTest(
    'HttpTransport rejects file:// baseUrl',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'the-http-transport-rejects-a-file-scheme-base-url',
    },
    () => {
      expect(() => new HttpTransport('file:///etc/passwd')).toThrow(/scheme/);
    },
  );

  specTest(
    'ConnectTransport rejects file:// baseUrl',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'the-connect-transport-rejects-a-file-scheme-base-url',
    },
    () => {
      expect(() => new ConnectTransport('file:///etc/passwd', 'pkg.v1')).toThrow(/scheme/);
    },
  );

  specTest(
    'WebSocketTransport rejects file:// baseUrl',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'the-websocket-transport-rejects-a-file-scheme-base-url',
    },
    () => {
      expect(() => new WebSocketTransport('file:///etc/passwd')).toThrow(/scheme/);
    },
  );

  test('HttpTransport still accepts valid https baseUrl', () => {
    expect(() => new HttpTransport('https://api.example.com')).not.toThrow();
  });
});

describe('BaseClient rejects non-http baseUrl', () => {
  test('throws on file:// baseUrl', () => {
    expect(() => new UrlTestClient({ baseUrl: 'file:///secret', transport: 'http' })).toThrow(/scheme/);
  });

  test('throws on malformed baseUrl', () => {
    expect(() => new UrlTestClient({ baseUrl: 'http://[invalid', transport: 'http' })).toThrow();
  });

  test('accepts a valid http baseUrl', () => {
    expect(() => new UrlTestClient({ baseUrl: 'http://localhost:3000', transport: 'http' })).not.toThrow();
  });
});
