import { describe, expect, it } from 'bun:test';
import { buildUrlWithParams, getBaseUrl, joinUrlPath, parseQueryString } from '../src';

describe('url.utils', () => {
  describe('buildUrlWithParams / buildUrl', () => {
    it('appends query parameters to URL', () => {
      const result = buildUrlWithParams('https://api.example.com/users', { page: 1, limit: 10 });
      expect(result).toBe('https://api.example.com/users?page=1&limit=10');
    });

    it('substitutes path parameters', () => {
      const result = buildUrlWithParams('https://api.example.com/users/[id]', { id: '123' });
      expect(result).toBe('https://api.example.com/users/123');
    });

    it('encodes path parameter values to prevent traversal/injection', () => {
      const result = buildUrlWithParams('https://api.example.com/files/[id]', { id: '../../etc/passwd?x=1' });
      expect(result).toBe('https://api.example.com/files/..%2F..%2Fetc%2Fpasswd%3Fx%3D1');
    });

    it('handles mixed path and query parameters', () => {
      const result = buildUrlWithParams('https://api.example.com/users/[id]/posts', { id: '123', page: 1 });
      expect(result).toBe('https://api.example.com/users/123/posts?page=1');
    });

    it('encodes query parameter values', () => {
      const result = buildUrlWithParams('https://example.com', { search: 'hello world' });
      expect(result).toBe('https://example.com?search=hello%20world');
    });

    it('skips null and undefined values', () => {
      const result = buildUrlWithParams('https://example.com', { a: 1, b: null, c: undefined });
      expect(result).toBe('https://example.com?a=1');
    });

    it('returns base URL if no params provided', () => {
      const result = buildUrlWithParams('https://example.com');
      expect(result).toBe('https://example.com');
    });
  });

  describe('parseQueryString / parseQuery', () => {
    it('parses query string to object', () => {
      const result = parseQueryString('page=1&limit=10');
      expect(result).toEqual({ page: '1', limit: '10' });
    });

    it('handles URL-encoded values', () => {
      const result = parseQueryString('search=hello%20world');
      expect(result).toEqual({ search: 'hello world' });
    });

    it('handles leading question mark', () => {
      const result = parseQueryString('?page=1');
      expect(result).toEqual({ page: '1' });
    });

    it('returns empty object for empty string', () => {
      const result = parseQueryString('');
      expect(result).toEqual({});
    });
  });

  describe('getBaseUrl', () => {
    it('removes query string and hash', () => {
      const result = getBaseUrl('https://example.com/path?query=1#hash');
      expect(result).toBe('https://example.com/path');
    });

    it('returns URL as-is if no query or hash', () => {
      const result = getBaseUrl('https://example.com/path');
      expect(result).toBe('https://example.com/path');
    });

    it('handles root URL', () => {
      const result = getBaseUrl('https://example.com');
      expect(result).toBe('https://example.com/');
    });

    it('handles invalid URLs gracefully', () => {
      const result = getBaseUrl('not-a-url?query=1');
      expect(result).toBe('not-a-url');
    });
  });

  describe('joinUrlPath', () => {
    it('joins path segments', () => {
      const result = joinUrlPath('https://api.example.com', 'users', '123');
      expect(result).toBe('https://api.example.com/users/123');
    });

    it('handles trailing slashes', () => {
      const result = joinUrlPath('/api/', '/users/', '/list');
      expect(result).toBe('/api/users/list');
    });

    it('handles empty segments', () => {
      const result = joinUrlPath('https://example.com', '', 'path');
      expect(result).toBe('https://example.com/path');
    });
  });
});
