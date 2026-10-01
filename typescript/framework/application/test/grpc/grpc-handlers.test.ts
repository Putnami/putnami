import { describe, expect, it } from 'bun:test';
import type { DiscoveredRoute } from '../../src/api';
import { buildUnimplementedRpcHandler, matchRpcToRoute } from '../../src/grpc/grpc-handlers';

function route(method: string, path: string): DiscoveredRoute {
  return {
    method,
    path,
    schemas: {},
  } as DiscoveredRoute;
}

describe('matchRpcToRoute', () => {
  const routes: DiscoveredRoute[] = [
    route('GET', '/users'),
    route('GET', '/users/[id]'),
    route('POST', '/users'),
    route('PUT', '/users/[id]'),
    route('PATCH', '/users/[id]'),
    route('DELETE', '/users/[id]'),
    route('GET', '/orders/[orderId]/items'),
    route('POST', '/auth/login'),
  ];

  it('matches GET list route', () => {
    const match = matchRpcToRoute('ListUsers', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('GET');
    expect(match?.path).toBe('/users');
  });

  it('matches GET single route with param', () => {
    const match = matchRpcToRoute('GetUsersById', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('GET');
    expect(match?.path).toBe('/users/[id]');
  });

  it('matches POST route', () => {
    const match = matchRpcToRoute('CreateUsers', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('POST');
    expect(match?.path).toBe('/users');
  });

  it('matches PUT route', () => {
    const match = matchRpcToRoute('UpdateUsersById', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('PUT');
  });

  it('matches PATCH route', () => {
    const match = matchRpcToRoute('PatchUsersById', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('PATCH');
  });

  it('matches DELETE route', () => {
    const match = matchRpcToRoute('DeleteUsersById', routes);
    expect(match).toBeDefined();
    expect(match?.method).toBe('DELETE');
  });

  it('matches nested path', () => {
    const match = matchRpcToRoute('ListOrdersByOrderIdItems', routes);
    expect(match).toBeDefined();
    expect(match?.path).toBe('/orders/[orderId]/items');
  });

  it('matches compound path', () => {
    const match = matchRpcToRoute('CreateAuthLogin', routes);
    expect(match).toBeDefined();
    expect(match?.path).toBe('/auth/login');
  });

  it('returns undefined for unknown RPC name', () => {
    const match = matchRpcToRoute('NonExistentRpc', routes);
    expect(match).toBeUndefined();
  });

  it('returns undefined for empty routes', () => {
    const match = matchRpcToRoute('ListUsers', []);
    expect(match).toBeUndefined();
  });
});

describe('buildUnimplementedRpcHandler', () => {
  function ctxWith(headers: Record<string, string>) {
    return { headers: new Headers(headers) } as never;
  }

  it('answers a Connect POST with gRPC status UNIMPLEMENTED (HTTP 501)', async () => {
    const handler = buildUnimplementedRpcHandler('bidirectional');
    const response = await handler(ctxWith({ 'content-type': 'application/json' }));
    expect(response.status).toBe(501);
    const body = response.rawData() as { code: string; message: string };
    expect(body.code).toBe('unimplemented');
    expect(body.message).toContain('bidirectional');
    expect(body.message).toContain('WebSocket');
  });

  it('answers a client-streaming RPC with the same status', async () => {
    const handler = buildUnimplementedRpcHandler('client');
    const response = await handler(ctxWith({ 'content-type': 'application/json' }));
    expect(response.status).toBe(501);
    expect((response.rawData() as { code: string }).code).toBe('unimplemented');
  });

  it('answers gRPC-Web callers with a 200 carrying grpc-status 12', async () => {
    const handler = buildUnimplementedRpcHandler('bidirectional');
    const response = await handler(
      ctxWith({ 'content-type': 'application/grpc-web+proto', accept: 'application/grpc-web+proto' }),
    );
    // gRPC-Web carries the status in the trailer frame of a 200 response, so
    // the HTTP layer stays ok and the trailer says UNIMPLEMENTED.
    expect(response.ok).toBe(true);
    expect(response.getHeader('Content-Type')).toBe('application/grpc-web+proto');
    const raw = new Uint8Array(await new Response(response.getBodyInit()).arrayBuffer());
    const text = new TextDecoder().decode(raw);
    expect(text).toContain('grpc-status: 12');
  });
});
