import { describe, expect, it } from 'bun:test';
import { HttpException } from '@putnami/runtime';
import { buildForwardHeaders, buildInternalUrl, readRequestEnvelope } from '../../src/grpc/grpc-request';
import type { DiscoveredRoute } from '../../src/api';
import type { HttpRequestContext } from '../../src/http/http-context.type';

describe('readRequestEnvelope', () => {
  it('reads the params, query and body sections', () => {
    const result = readRequestEnvelope({
      params: { id: '123' },
      query: { page: 1 },
      body: { name: 'test', id: 'the body keeps its own id' },
    });

    expect(result.params).toEqual({ id: '123' });
    expect(result.query).toEqual({ page: '1' });
    // A body member named like a path parameter stays in the body.
    expect(result.body).toEqual({ name: 'test', id: 'the body keeps its own id' });
  });

  it('converts snake_case keys of path and query values to camelCase', () => {
    const result = readRequestEnvelope({ params: { user_id: '42' }, query: { page_size: 10 } });
    expect(result.params).toEqual({ userId: '42' });
    expect(result.query).toEqual({ pageSize: '10' });
  });

  it('reads an absent section as empty and ignores a member that names no section', () => {
    const result = readRequestEnvelope({ unknown: 'ignored' });
    expect(result).toEqual({ params: {}, query: {}, body: {} });
  });

  it('refuses a section that is not an object', () => {
    expect(() => readRequestEnvelope({ params: 'id=1' })).toThrow(HttpException);
    expect(() => readRequestEnvelope({ body: [1, 2] })).toThrow(/carries "body" as a non-object/);
  });
});

describe('buildInternalUrl', () => {
  it('builds URL without params or query', () => {
    const route = { method: 'GET', path: '/users' } as DiscoveredRoute;
    const url = buildInternalUrl(route, {}, {});
    expect(url).toBe('http://internal/users');
  });

  it('substitutes path params', () => {
    const route = { method: 'GET', path: '/users/[id]' } as DiscoveredRoute;
    const url = buildInternalUrl(route, { id: '42' }, {});
    expect(url).toBe('http://internal/users/42');
  });

  it('appends query string', () => {
    const route = { method: 'GET', path: '/users' } as DiscoveredRoute;
    const url = buildInternalUrl(route, {}, { page: '2', limit: '10' });
    expect(url).toBe('http://internal/users?page=2&limit=10');
  });

  it('combines params and query', () => {
    const route = { method: 'GET', path: '/orgs/[orgId]/members' } as DiscoveredRoute;
    const url = buildInternalUrl(route, { orgId: 'abc' }, { role: 'admin' });
    expect(url).toBe('http://internal/orgs/abc/members?role=admin');
  });

  it('encodes special characters in params', () => {
    const route = { method: 'GET', path: '/users/[name]' } as DiscoveredRoute;
    const url = buildInternalUrl(route, { name: 'foo bar' }, {});
    expect(url).toContain('foo%20bar');
  });

  it('filters empty query values', () => {
    const route = { method: 'GET', path: '/users' } as DiscoveredRoute;
    const url = buildInternalUrl(route, {}, { name: 'test', empty: '' });
    expect(url).toBe('http://internal/users?name=test');
  });
});

describe('buildForwardHeaders', () => {
  it('sets default content-type and accept headers', () => {
    const ctx = {
      headers: new Headers(),
    } as unknown as HttpRequestContext;

    const headers = buildForwardHeaders(ctx);
    expect(headers['Accept']).toBe('application/json');
    expect(headers['Content-Type']).toBe('application/json');
  });

  it('forwards custom headers', () => {
    const ctx = {
      headers: new Headers({
        Authorization: 'Bearer token123',
        'X-Custom': 'value',
      }),
    } as unknown as HttpRequestContext;

    const headers = buildForwardHeaders(ctx);
    expect(headers['authorization']).toBe('Bearer token123');
    expect(headers['x-custom']).toBe('value');
  });

  it('skips hop-by-hop headers', () => {
    const ctx = {
      headers: new Headers({
        host: 'example.com',
        connection: 'keep-alive',
        'transfer-encoding': 'chunked',
        'content-length': '42',
      }),
    } as unknown as HttpRequestContext;

    const headers = buildForwardHeaders(ctx);
    expect(headers['host']).toBeUndefined();
    expect(headers['connection']).toBeUndefined();
    expect(headers['transfer-encoding']).toBeUndefined();
  });

  it('skips gRPC-specific headers', () => {
    const ctx = {
      headers: new Headers({
        'grpc-timeout': '10S',
        'grpc-accept-encoding': 'gzip',
        'grpc-encoding': 'gzip',
      }),
    } as unknown as HttpRequestContext;

    const headers = buildForwardHeaders(ctx);
    expect(headers['grpc-timeout']).toBeUndefined();
    expect(headers['grpc-accept-encoding']).toBeUndefined();
    expect(headers['grpc-encoding']).toBeUndefined();
  });
});
