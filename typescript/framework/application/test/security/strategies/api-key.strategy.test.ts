import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { PrincipalKind } from '../../../src/security/identity.constants';
import { apiKeyStrategy } from '../../../src/security/strategies/api-key.strategy';
import { resolveScopes } from '../../../src/security/security.utils';

const configuredKey = {
  feature: 'typescript/authentication-authorization',
  requirement: 'strategy-composition',
  check: 'an-api-key-matches-only-a-configured-key',
};

// TypeScript twin of go/framework/security/apikey_test.go.

function ctxWithHeaders(headers: Record<string, string>): HttpRequestContext {
  return { req: new Request('http://localhost/v1/traces', { method: 'POST', headers }) } as HttpRequestContext;
}

describe('apiKeyStrategy', () => {
  specTest('authenticates a valid key', configuredKey, () => {
    const strategy = apiKeyStrategy({ keys: ['key-1', 'key-2'] });
    const principal = strategy(ctxWithHeaders({ 'X-Api-Key': 'key-1' }));
    expect(principal).toBeDefined();
    expect(principal?.sub).toBe('apikey');
    expect(principal?.kind).toBe(PrincipalKind.ApiKey);
  });

  specTest('rejects an invalid key', configuredKey, () => {
    const strategy = apiKeyStrategy({ keys: ['key-1'] });
    expect(strategy(ctxWithHeaders({ 'X-Api-Key': 'wrong-key' }))).toBeUndefined();
  });

  specTest('rejects when the header is absent', configuredKey, () => {
    const strategy = apiKeyStrategy({ keys: ['key-1'] });
    expect(strategy(ctxWithHeaders({}))).toBeUndefined();
  });

  specTest('reads the key from a custom header', configuredKey, () => {
    const strategy = apiKeyStrategy({ keys: ['secret'], header: 'X-Custom-Key' });
    expect(strategy(ctxWithHeaders({ 'X-Custom-Key': 'secret' }))).toBeDefined();
    // The default header is not consulted when a custom one is configured.
    expect(strategy(ctxWithHeaders({ 'X-Api-Key': 'secret' }))).toBeUndefined();
  });

  it('honors a custom subject', () => {
    const strategy = apiKeyStrategy({ keys: ['key-1'], subject: 'service-account' });
    const principal = strategy(ctxWithHeaders({ 'X-Api-Key': 'key-1' }));
    expect(principal?.sub).toBe('service-account');
  });

  it('grants configured scopes', () => {
    const strategy = apiKeyStrategy({ keys: ['key-1'], scopes: ['ingest', 'read'] });
    const principal = strategy(ctxWithHeaders({ 'X-Api-Key': 'key-1' }));
    expect(principal).toBeDefined();
    // Scopes surface through the standard `scope` claim resolution, like a JWT.
    expect(resolveScopes(principal ?? {}, {})).toEqual(['ingest', 'read']);
  });

  specTest(
    'does not leak a match via key length (constant-time compare handles mismatched lengths)',
    configuredKey,
    () => {
      const strategy = apiKeyStrategy({ keys: ['a-fairly-long-secret-key'] });
      // A far-shorter presented key must simply fail, never throw on length mismatch.
      expect(strategy(ctxWithHeaders({ 'X-Api-Key': 'x' }))).toBeUndefined();
    },
  );
});
