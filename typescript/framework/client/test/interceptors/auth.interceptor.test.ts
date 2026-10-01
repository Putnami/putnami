import { describe, expect, test } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { authInterceptor, CLIENT_ID_HEADER } from '../../src/interceptors/auth.interceptor';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function makeRequest(): ClientRequest {
  return { method: 'GET', path: '/test', headers: new Headers() };
}

function makeResponse(): ClientResponse {
  return { data: { ok: true }, status: 200, headers: new Headers() };
}

describe('authInterceptor', () => {
  test('does nothing when no auth context and no token provider', async () => {
    const interceptor = authInterceptor();
    const request = makeRequest();

    await interceptor(request, async (req) => {
      expect(req.headers.has('Authorization')).toBe(false);
      return makeResponse();
    });
  });

  test('does not override existing Authorization header', async () => {
    const interceptor = authInterceptor({
      tokenProvider: async () => 'should-not-be-used',
    });
    const request = makeRequest();
    request.headers.set('Authorization', 'Bearer existing-token');

    await interceptor(request, async (req) => {
      expect(req.headers.get('Authorization')).toBe('Bearer existing-token');
      return makeResponse();
    });
  });

  test('forwards JWT from async context (__authorizationHeader)', async () => {
    const interceptor = authInterceptor();

    await runInContext({ __authorizationHeader: 'Bearer user-jwt-token' }, async () => {
      const request = makeRequest();
      await interceptor(request, async (req) => {
        expect(req.headers.get('Authorization')).toBe('Bearer user-jwt-token');
        return makeResponse();
      });
    });
  });

  test('uses tokenProvider when no context JWT', async () => {
    const interceptor = authInterceptor({
      tokenProvider: async () => 'client-credentials-token',
    });
    const request = makeRequest();

    await interceptor(request, async (req) => {
      expect(req.headers.get('Authorization')).toBe('Bearer client-credentials-token');
      return makeResponse();
    });
  });

  test('context JWT takes priority over tokenProvider', async () => {
    const interceptor = authInterceptor({
      tokenProvider: async () => 'should-not-be-used',
    });

    await runInContext({ __authorizationHeader: 'Bearer user-jwt' }, async () => {
      const request = makeRequest();
      await interceptor(request, async (req) => {
        expect(req.headers.get('Authorization')).toBe('Bearer user-jwt');
        return makeResponse();
      });
    });
  });

  test('injects X-Client-Id header', async () => {
    const interceptor = authInterceptor({ clientId: 'orders-service' });
    const request = makeRequest();

    await interceptor(request, async (req) => {
      expect(req.headers.get(CLIENT_ID_HEADER)).toBe('orders-service');
      return makeResponse();
    });
  });

  test('does not override existing X-Client-Id', async () => {
    const interceptor = authInterceptor({ clientId: 'should-not-override' });
    const request = makeRequest();
    request.headers.set(CLIENT_ID_HEADER, 'original-client');

    await interceptor(request, async (req) => {
      expect(req.headers.get(CLIENT_ID_HEADER)).toBe('original-client');
      return makeResponse();
    });
  });

  test('backward compat: accepts function as first arg', async () => {
    const interceptor = authInterceptor(async () => 'compat-token');
    const request = makeRequest();

    await interceptor(request, async (req) => {
      expect(req.headers.get('Authorization')).toBe('Bearer compat-token');
      return makeResponse();
    });
  });

  test('handles tokenProvider returning undefined', async () => {
    const interceptor = authInterceptor({
      tokenProvider: async () => undefined,
    });
    const request = makeRequest();

    await interceptor(request, async (req) => {
      expect(req.headers.has('Authorization')).toBe(false);
      return makeResponse();
    });
  });
});
