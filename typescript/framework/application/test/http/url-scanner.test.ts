import { describe, expect, it } from 'bun:test';
import { UrlScanner } from '../../src/http/url.scanner';

describe('UrlScanner', () => {
  describe('secured', () => {
    it('returns true for https URLs', () => {
      const scanner = new UrlScanner('https://example.com/path');
      expect(scanner.secured).toBe(true);
    });

    it('returns false for http URLs', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.secured).toBe(false);
    });

    it('checks x-forwarded-proto header for relative URLs', () => {
      const headers = new Headers({ 'x-forwarded-proto': 'https' });
      const scanner = new UrlScanner('/path', headers);
      expect(scanner.secured).toBe(true);
    });

    it('honours x-forwarded-proto for plaintext http URLs behind a TLS proxy', () => {
      const headers = new Headers({ 'x-forwarded-proto': 'https' });
      const scanner = new UrlScanner('http://example.com/path', headers);
      expect(scanner.secured).toBe(true);
    });

    it('stays insecure for http URLs when x-forwarded-proto is not https', () => {
      const headers = new Headers({ 'x-forwarded-proto': 'http' });
      const scanner = new UrlScanner('http://example.com/path', headers);
      expect(scanner.secured).toBe(false);
    });

    it('returns false for relative URL without forwarded header', () => {
      const scanner = new UrlScanner('/path');
      expect(scanner.secured).toBe(false);
    });

    it('caches the result on subsequent calls', () => {
      const scanner = new UrlScanner('https://example.com/path');
      expect(scanner.secured).toBe(true);
      expect(scanner.secured).toBe(true); // cached
    });
  });

  describe('host', () => {
    it('extracts host from absolute URL with path', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.host).toBe('example.com');
    });

    it('extracts host from absolute URL without trailing slash', () => {
      const scanner = new UrlScanner('http://example.com');
      expect(scanner.host).toBe('example.com');
    });

    it('extracts host from URL with query but no path', () => {
      const scanner = new UrlScanner('http://example.com?foo=bar');
      expect(scanner.host).toBe('example.com');
    });

    it('extracts host with port', () => {
      const scanner = new UrlScanner('http://localhost:3000/path');
      expect(scanner.host).toBe('localhost:3000');
    });

    it('falls back to host header for relative URLs', () => {
      const headers = new Headers({ host: 'example.com' });
      const scanner = new UrlScanner('/path', headers);
      expect(scanner.host).toBe('example.com');
    });

    it('falls back to x-forwarded-host header', () => {
      const headers = new Headers({ 'x-forwarded-host': 'proxy.example.com' });
      const scanner = new UrlScanner('/path', headers);
      expect(scanner.host).toBe('proxy.example.com');
    });

    it('returns empty string when no host can be determined', () => {
      const scanner = new UrlScanner('/path');
      expect(scanner.host).toBe('');
    });

    it('caches the result', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.host).toBe('example.com');
      expect(scanner.host).toBe('example.com');
    });
  });

  describe('domain', () => {
    it('builds http domain from host', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.domain).toBe('http://example.com');
    });

    it('builds https domain from host', () => {
      const scanner = new UrlScanner('https://example.com/path');
      expect(scanner.domain).toBe('https://example.com');
    });

    it('caches the result', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.domain).toBe('http://example.com');
      expect(scanner.domain).toBe('http://example.com');
    });
  });

  describe('path', () => {
    it('extracts path from absolute URL', () => {
      const scanner = new UrlScanner('http://example.com/api/users');
      expect(scanner.path).toBe('api/users');
    });

    it('strips query string from path', () => {
      const scanner = new UrlScanner('http://example.com/api/users?page=1');
      expect(scanner.path).toBe('api/users');
    });

    it('returns empty string for root path', () => {
      const scanner = new UrlScanner('http://example.com/');
      expect(scanner.path).toBe('');
    });

    it('returns / as empty for URL without path', () => {
      const scanner = new UrlScanner('http://example.com');
      expect(scanner.path).toBe('');
    });

    it('returns / as empty for URL without path but with query', () => {
      const scanner = new UrlScanner('http://example.com?foo=bar');
      expect(scanner.path).toBe('');
    });

    it('strips leading slash from relative URLs', () => {
      const scanner = new UrlScanner('/api/users');
      expect(scanner.path).toBe('api/users');
    });

    it('strips leading slash and query from relative URLs', () => {
      const scanner = new UrlScanner('/api/users?page=1');
      expect(scanner.path).toBe('api/users');
    });

    it('caches the result', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.path).toBe('path');
      expect(scanner.path).toBe('path');
    });
  });

  describe('query', () => {
    it('extracts query string including ?', () => {
      const scanner = new UrlScanner('http://example.com/path?foo=bar&baz=1');
      expect(scanner.query).toBe('?foo=bar&baz=1');
    });

    it('returns empty string when no query', () => {
      const scanner = new UrlScanner('http://example.com/path');
      expect(scanner.query).toBe('');
    });

    it('caches the result', () => {
      const scanner = new UrlScanner('http://example.com/path?foo=bar');
      expect(scanner.query).toBe('?foo=bar');
      expect(scanner.query).toBe('?foo=bar');
    });
  });
});
