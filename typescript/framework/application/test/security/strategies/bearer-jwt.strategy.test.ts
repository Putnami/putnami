import { afterEach, describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { type OAuthService, setActiveOAuthService } from '../../../src/oauth/oauth.service';
import { bearerJwtStrategy } from '../../../src/security/strategies/bearer-jwt.strategy';

// TypeScript twin of go/framework/security/jwt_test.go. The bearer-JWT strategy
// wraps the active OAuthService's existing RS256/JWKS verifier, so these tests
// stub OAuthService.verify and assert the strategy's contract around it (token
// extraction, claim pass-through, issuer/audience forwarding, fail-closed).

function bearerCtx(token?: string): HttpRequestContext {
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  return { req: new Request('http://localhost/v1/traces', { method: 'POST', headers }) } as HttpRequestContext;
}

interface VerifyCall {
  token: string;
  options: Record<string, unknown>;
}

function stubService(verify: (token: string, options: Record<string, unknown>) => unknown): VerifyCall[] {
  const calls: VerifyCall[] = [];
  setActiveOAuthService({
    verify: async (token: string, options: Record<string, unknown>) => {
      calls.push({ token, options });
      return verify(token, options);
    },
  } as unknown as OAuthService);
  return calls;
}

describe('bearerJwtStrategy', () => {
  afterEach(() => setActiveOAuthService(undefined));

  it('resolves the verified claims as a principal', async () => {
    stubService(() => ({ sub: 'service-a', scope: 'ingest', iss: 'https://auth.example.com' }));
    const principal = await bearerJwtStrategy()(bearerCtx('good.jwt.token'));
    expect(principal?.sub).toBe('service-a');
    expect(principal?.iss).toBe('https://auth.example.com');
    // A bearer JWT is a `user` principal: `.secure({ principalKind: 'user' })`
    // must be satisfiable via this strategy.
    expect(principal?.kind).toBe('user');
  });

  it('defaults the principal kind to "user" but preserves a token-provided kind', async () => {
    stubService(() => ({ sub: 'u' }));
    expect((await bearerJwtStrategy()(bearerCtx('t.o.k')))?.kind).toBe('user');
    // A `kind` claim carried by the token itself wins over the default.
    stubService(() => ({ sub: 'u', kind: 'apikey' }));
    expect((await bearerJwtStrategy()(bearerCtx('t.o.k')))?.kind).toBe('apikey');
  });

  it('resolves nothing when verification fails (expired / bad signature / malformed)', async () => {
    stubService(() => undefined); // OAuthService.verify returns undefined on any invalid token
    expect(await bearerJwtStrategy()(bearerCtx('expired.jwt.token'))).toBeUndefined();
  });

  it('resolves nothing when there is no bearer header', async () => {
    const calls = stubService(() => ({ sub: 'x' }));
    expect(await bearerJwtStrategy()(bearerCtx())).toBeUndefined();
    expect(calls).toHaveLength(0); // verify is never called without a token
  });

  it('fails closed when no OAuthService is registered', async () => {
    setActiveOAuthService(undefined);
    expect(await bearerJwtStrategy()(bearerCtx('some.jwt.token'))).toBeUndefined();
  });

  it('fails closed when verify throws (e.g. JWKS endpoint unconfigured)', async () => {
    stubService(() => {
      throw new Error('JWKS endpoint is not configured');
    });
    expect(await bearerJwtStrategy()(bearerCtx('some.jwt.token'))).toBeUndefined();
  });

  it('forwards issuer and audience constraints to verify', async () => {
    const calls = stubService(() => ({ sub: 'u' }));
    await bearerJwtStrategy({ issuer: 'https://auth.example.com', audience: 'billing' })(bearerCtx('t.o.k'));
    expect(calls[0]?.options).toEqual({ issuer: 'https://auth.example.com', audience: 'billing' });
  });

  it('passes no issuer/audience when none configured (signature + exp only)', async () => {
    const calls = stubService(() => ({ sub: 'u' }));
    await bearerJwtStrategy()(bearerCtx('t.o.k'));
    expect(calls[0]?.options).toEqual({});
  });
});
