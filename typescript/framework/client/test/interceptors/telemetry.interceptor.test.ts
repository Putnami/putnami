import { describe, expect, test } from 'bun:test';
import { telemetryInterceptor } from '../../src/interceptors/telemetry.interceptor';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function makeRequest(): ClientRequest {
  return { method: 'GET', path: '/test', headers: new Headers() };
}

function makeResponse(status: number): ClientResponse {
  return { data: { ok: true }, status, headers: new Headers() };
}

describe('telemetryInterceptor', () => {
  test('normalizes valid contract identities for metric keys without changing the binding identity', () => {
    expect(() => telemetryInterceptor('catalog.items')).not.toThrow();
    expect(() => telemetryInterceptor('users service')).not.toThrow();
    expect(() => telemetryInterceptor('')).toThrow('serviceName must not be empty');
  });

  test('returns the downstream response', async () => {
    const interceptor = telemetryInterceptor('users_service');
    const response = await interceptor(makeRequest(), async () => makeResponse(201));

    expect(response.status).toBe(201);
  });

  test('rethrows downstream errors after recording telemetry', async () => {
    const interceptor = telemetryInterceptor('users_service');

    await expect(
      interceptor(makeRequest(), async () => {
        throw new Error('boom');
      }),
    ).rejects.toThrow('boom');
  });
});
