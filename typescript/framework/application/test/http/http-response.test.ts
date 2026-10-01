import { describe, expect, it } from 'bun:test';
import {
  HttpResponse,
  json,
  badRequest,
  unauthorized,
  forbidden,
  notFound,
  internalServerError,
} from '../../src/http/http-response';

describe('HttpResponse.json()', () => {
  it('should set Content-Type to application/json', () => {
    const res = HttpResponse.json({ ok: true });
    expect(res.getHeader('Content-Type')).toBe('application/json');
  });

  it('should default status to 200', () => {
    const res = HttpResponse.json({ ok: true });
    expect(res.status).toBe(200);
  });

  it('should allow overriding status', () => {
    const res = HttpResponse.json({ error: 'not found' }, { status: 404 });
    expect(res.status).toBe(404);
  });

  it('should preserve caller-provided headers (record form)', () => {
    const res = HttpResponse.json({ ok: true }, { headers: { 'Cache-Control': 'no-store', 'X-Custom': 'value' } });
    expect(res.getHeader('Cache-Control')).toBe('no-store');
    expect(res.getHeader('X-Custom')).toBe('value');
    expect(res.getHeader('Content-Type')).toBe('application/json');
  });

  it('should preserve caller-provided headers (tuple array form)', () => {
    const res = HttpResponse.json(
      { ok: true },
      {
        headers: [
          ['Cache-Control', 'no-store'],
          ['X-Custom', 'value'],
        ],
      },
    );
    expect(res.getHeader('Cache-Control')).toBe('no-store');
    expect(res.getHeader('X-Custom')).toBe('value');
    expect(res.getHeader('Content-Type')).toBe('application/json');
  });

  it('should let Content-Type: application/json win over caller Content-Type', () => {
    const res = HttpResponse.json({ ok: true }, { headers: { 'Content-Type': 'text/plain' } });
    expect(res.getHeader('Content-Type')).toBe('application/json');
  });

  it('should expose rawData()', () => {
    const data = { id: 1, name: 'test' };
    const res = HttpResponse.json(data);
    expect(res.rawData()).toBe(data);
  });
});

describe('json() standalone function', () => {
  it('should delegate to HttpResponse.json()', () => {
    const res = json({ ok: true }, { status: 201, headers: { 'X-Req-Id': '123' } });
    expect(res.status).toBe(201);
    expect(res.getHeader('X-Req-Id')).toBe('123');
    expect(res.getHeader('Content-Type')).toBe('application/json');
  });
});

describe('error helpers with optional body', () => {
  it('should return default body when no argument', () => {
    expect(badRequest().status).toBe(400);
    expect(badRequest().rawData()).toEqual({ error: 'Bad Request' });

    expect(unauthorized().status).toBe(401);
    expect(unauthorized().rawData()).toEqual({ error: 'Unauthorized' });

    expect(forbidden().status).toBe(403);
    expect(forbidden().rawData()).toEqual({ error: 'Forbidden' });

    expect(notFound().status).toBe(404);
    expect(notFound().rawData()).toEqual({ error: 'Not Found' });

    expect(internalServerError().status).toBe(500);
    expect(internalServerError().rawData()).toEqual({ error: 'Internal Server Error' });
  });

  it('should use custom body when provided', () => {
    const body = { error: 'invalid_client', error_description: 'Invalid credentials' };
    const res = unauthorized(body);
    expect(res.status).toBe(401);
    expect(res.rawData()).toEqual(body);
  });

  it('should work with static methods too', () => {
    const body = { error: 'access_denied', reason: 'scope mismatch' };
    const res = HttpResponse.forbidden(body);
    expect(res.status).toBe(403);
    expect(res.rawData()).toEqual(body);
  });
});
