import { describe, expect, it } from 'bun:test';
import { redirect } from '../../src/http/redirect.utils';
import { HttpResponse } from '../../src/http/http-response';

describe('redirect', () => {
  it('throws an HttpResponse redirect with default 302 status', () => {
    try {
      redirect('/login');
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(HttpResponse);
      const response = e as HttpResponse;
      expect(response.status).toBe(302);
      expect(response.getHeader('Location')).toBe('/login');
    }
  });

  it('throws an HttpResponse redirect with 301 status', () => {
    try {
      redirect('/new-url', 301);
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(HttpResponse);
      const response = e as HttpResponse;
      expect(response.status).toBe(301);
      expect(response.getHeader('Location')).toBe('/new-url');
    }
  });

  it('throws an HttpResponse redirect with 307 status', () => {
    try {
      redirect('/temporary', 307);
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(HttpResponse);
      const response = e as HttpResponse;
      expect(response.status).toBe(307);
    }
  });

  it('throws an HttpResponse redirect with 308 status', () => {
    try {
      redirect('/permanent', 308);
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(HttpResponse);
      const response = e as HttpResponse;
      expect(response.status).toBe(308);
    }
  });
});
