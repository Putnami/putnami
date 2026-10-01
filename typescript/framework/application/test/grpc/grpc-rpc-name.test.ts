import { describe, expect, it } from 'bun:test';
import type { DiscoveredRoute } from '../../src/api';
import { matchRpcToRoute } from '../../src/grpc/grpc-handlers';

function route(method: string, path: string): DiscoveredRoute {
  return { method, path, schemas: {} } as DiscoveredRoute;
}

describe('buildRpcName (tested via matchRpcToRoute)', () => {
  it('uses List prefix for GET without trailing param', () => {
    const routes = [route('GET', '/products')];
    expect(matchRpcToRoute('ListProducts', routes)).toBeDefined();
  });

  it('uses Get prefix for GET with trailing param', () => {
    const routes = [route('GET', '/products/[id]')];
    expect(matchRpcToRoute('GetProductsById', routes)).toBeDefined();
  });

  it('uses Create prefix for POST', () => {
    const routes = [route('POST', '/products')];
    expect(matchRpcToRoute('CreateProducts', routes)).toBeDefined();
  });

  it('uses Update prefix for PUT', () => {
    const routes = [route('PUT', '/products/[id]')];
    expect(matchRpcToRoute('UpdateProductsById', routes)).toBeDefined();
  });

  it('uses Patch prefix for PATCH', () => {
    const routes = [route('PATCH', '/products/[id]')];
    expect(matchRpcToRoute('PatchProductsById', routes)).toBeDefined();
  });

  it('uses Delete prefix for DELETE', () => {
    const routes = [route('DELETE', '/products/[id]')];
    expect(matchRpcToRoute('DeleteProductsById', routes)).toBeDefined();
  });

  it('uses PascalCase method name for unknown methods', () => {
    const routes = [route('OPTIONS', '/health')];
    // 'OPTIONS' uppercased → pascalCase('OPTIONS') = 'OPTIONS' (already caps)
    expect(matchRpcToRoute('OPTIONSHealth', routes)).toBeDefined();
  });

  it('handles multi-segment paths', () => {
    const routes = [route('GET', '/api/v1/users')];
    expect(matchRpcToRoute('ListApiV1Users', routes)).toBeDefined();
  });

  it('handles nested path params', () => {
    const routes = [route('GET', '/org/[orgId]/members/[memberId]')];
    expect(matchRpcToRoute('GetOrgByOrgIdMembersByMemberId', routes)).toBeDefined();
  });

  it('handles root path', () => {
    const routes = [route('GET', '/')];
    // Empty segments after filter → no parts
    expect(matchRpcToRoute('List', routes)).toBeDefined();
  });
});
