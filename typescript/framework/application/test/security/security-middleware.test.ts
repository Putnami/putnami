import { describe, expect, it } from 'bun:test';
import { SecurityMiddleware } from '../../src/security/security.middleware';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import type { HttpResponse } from '../../src/http/http-response';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockContext(user?: Record<string, unknown>, params?: Record<string, string>): HttpRequestContext {
  return {
    user,
    params,
    req: new Request('http://localhost/test'),
    url: 'http://localhost/test',
    method: 'GET',
    headers: new Headers(),
    queryParams: () => ({}),
    body: async () => undefined,
    secured: () => false,
    host: () => 'localhost',
    domain: () => 'localhost',
    path: () => '/test',
    query: () => '',
    throw: (status: number, message?: string) => {
      throw new Error(message ?? `${status}`);
    },
  } as HttpRequestContext;
}

async function runMiddleware(
  // biome-ignore lint/suspicious/noExplicitAny: Test helper
  middleware: any,
  ctx: HttpRequestContext,
): Promise<{ status: number | undefined; passedThrough: boolean }> {
  let passedThrough = false;
  const next = async () => {
    passedThrough = true;
    return undefined;
  };

  const result = (await middleware(ctx, next)) as HttpResponse | undefined;
  return {
    status: result?.status,
    passedThrough,
  };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('SecurityMiddleware', () => {
  describe('no options (require any authenticated user)', () => {
    const mw = SecurityMiddleware();

    it('should return 401 when ctx.user is undefined', async () => {
      const ctx = createMockContext(undefined);
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(401);
      expect(result.passedThrough).toBe(false);
    });

    it('should pass through when ctx.user is present', async () => {
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('empty options (require any authenticated user)', () => {
    const mw = SecurityMiddleware({});

    it('should return 401 when no user', async () => {
      const ctx = createMockContext(undefined);
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(401);
    });

    it('should pass when user exists', async () => {
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('roles (require ALL)', () => {
    const mw = SecurityMiddleware({ roles: ['admin', 'editor'] });

    it('should return 403 when user has no roles', async () => {
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when user has only some required roles', async () => {
      const ctx = createMockContext({ sub: 'user-1', roles: ['admin'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when user has all required roles', async () => {
      const ctx = createMockContext({ sub: 'user-1', roles: ['admin', 'editor'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should pass when user has required roles and more', async () => {
      const ctx = createMockContext({ sub: 'user-1', roles: ['admin', 'editor', 'viewer'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('rolesAny (require ANY)', () => {
    const mw = SecurityMiddleware({ rolesAny: ['admin', 'editor'] });

    it('should return 403 when user has no matching role', async () => {
      const ctx = createMockContext({ sub: 'user-1', roles: ['viewer'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when user has one of the required roles', async () => {
      const ctx = createMockContext({ sub: 'user-1', roles: ['editor'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('scopes (require ALL)', () => {
    const mw = SecurityMiddleware({ scopes: ['read', 'write'] });

    it('should return 403 when user has no scopes', async () => {
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when user has only some required scopes', async () => {
      const ctx = createMockContext({ sub: 'user-1', scope: 'read' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when user has all required scopes', async () => {
      const ctx = createMockContext({ sub: 'user-1', scope: 'read write' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('scopesAny (require ANY)', () => {
    const mw = SecurityMiddleware({ scopesAny: ['read', 'write'] });

    it('should return 403 when user has no matching scope', async () => {
      const ctx = createMockContext({ sub: 'user-1', scope: 'delete' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when user has one matching scope', async () => {
      const ctx = createMockContext({ sub: 'user-1', scope: 'write' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('client', () => {
    const mw = SecurityMiddleware({ client: 'my-app' });

    it('should return 403 when token has no client claim', async () => {
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when azp matches', async () => {
      const ctx = createMockContext({ sub: 'user-1', azp: 'my-app' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should pass when client_id matches', async () => {
      const ctx = createMockContext({ sub: 'user-1', client_id: 'my-app' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should pass when aud matches', async () => {
      const ctx = createMockContext({ sub: 'user-1', aud: 'my-app' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should pass when aud array contains client', async () => {
      const ctx = createMockContext({ sub: 'user-1', aud: ['my-app', 'other'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should return 403 when client does not match', async () => {
      const ctx = createMockContext({ sub: 'user-1', azp: 'other-app' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });
  });

  describe('combined options', () => {
    const mw = SecurityMiddleware({
      roles: ['admin'],
      scopes: ['write'],
      client: 'my-app',
    });

    it('should pass when all requirements are met', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        roles: ['admin'],
        scope: 'read write',
        azp: 'my-app',
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should return 403 when roles fail but scopes and client pass', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        roles: ['viewer'],
        scope: 'read write',
        azp: 'my-app',
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when scopes fail', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        roles: ['admin'],
        scope: 'read',
        azp: 'my-app',
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when client fails', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        roles: ['admin'],
        scope: 'read write',
        azp: 'other-app',
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });
  });

  describe('custom guard function', () => {
    it('should return 401 when user is undefined', async () => {
      const mw = SecurityMiddleware(() => true);
      const ctx = createMockContext(undefined);
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(401);
    });

    it('should return 403 when guard returns false', async () => {
      const mw = SecurityMiddleware(() => false);
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass when guard returns true', async () => {
      const mw = SecurityMiddleware(() => true);
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should receive user claims and context', async () => {
      const mw = SecurityMiddleware((user, ctx) => user.orgId === ctx.params?.orgId);

      const passCtx = createMockContext({ sub: 'user-1', orgId: 'org-42' }, { orgId: 'org-42' });
      const passResult = await runMiddleware(mw, passCtx);
      expect(passResult.passedThrough).toBe(true);

      const failCtx = createMockContext({ sub: 'user-1', orgId: 'org-42' }, { orgId: 'org-99' });
      const failResult = await runMiddleware(mw, failCtx);
      expect(failResult.status).toBe(403);
    });

    it('should support async guard functions', async () => {
      const mw = SecurityMiddleware(async (user) => {
        await Promise.resolve();
        return user.role === 'admin';
      });

      const passCtx = createMockContext({ sub: 'user-1', role: 'admin' });
      const passResult = await runMiddleware(mw, passCtx);
      expect(passResult.passedThrough).toBe(true);

      const failCtx = createMockContext({ sub: 'user-1', role: 'viewer' });
      const failResult = await runMiddleware(mw, failCtx);
      expect(failResult.status).toBe(403);
    });
  });

  describe('Keycloak realm_access.roles', () => {
    const mw = SecurityMiddleware({ roles: ['admin'] });

    it('should resolve roles from realm_access.roles', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        realm_access: { roles: ['admin', 'user'] },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('Keycloak resource_access (client-scoped roles)', () => {
    const mw = SecurityMiddleware({ client: 'my-app', roles: ['editor'] });

    it('should resolve client-scoped roles from resource_access', async () => {
      const ctx = createMockContext({
        sub: 'user-1',
        azp: 'my-app',
        resource_access: {
          'my-app': { roles: ['editor', 'viewer'] },
        },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('scope claim formats', () => {
    const mw = SecurityMiddleware({ scopes: ['read'] });

    it('should handle space-separated scope string', async () => {
      const ctx = createMockContext({ sub: 'u', scope: 'read write' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should handle scp array claim', async () => {
      const ctx = createMockContext({ sub: 'u', scp: ['read', 'write'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should handle scopes array claim', async () => {
      const ctx = createMockContext({ sub: 'u', scopes: ['read', 'write'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('issuer binding', () => {
    const mw = SecurityMiddleware({ issuer: 'https://idp.example.com' });

    it('should pass when iss matches', async () => {
      const ctx = createMockContext({ sub: 'u', iss: 'https://idp.example.com' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should return 403 when iss does not match', async () => {
      const ctx = createMockContext({ sub: 'u', iss: 'https://evil.example.com' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when iss claim is absent (fail-closed)', async () => {
      const ctx = createMockContext({ sub: 'u' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should accept any of multiple expected issuers', async () => {
      const multi = SecurityMiddleware({ issuer: ['https://a.example.com', 'https://b.example.com'] });
      const ctx = createMockContext({ sub: 'u', iss: 'https://b.example.com' });
      const result = await runMiddleware(multi, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });

  describe('audience binding', () => {
    const mw = SecurityMiddleware({ audience: 'my-api' });

    it('should pass when aud matches (string)', async () => {
      const ctx = createMockContext({ sub: 'u', aud: 'my-api' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should pass when aud array contains the expected audience', async () => {
      const ctx = createMockContext({ sub: 'u', aud: ['other', 'my-api'] });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should return 403 when aud does not match', async () => {
      const ctx = createMockContext({ sub: 'u', aud: 'sibling-api' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should return 403 when aud claim is absent (fail-closed)', async () => {
      const ctx = createMockContext({ sub: 'u' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });
  });

  describe('principalKind requirement', () => {
    it('should pass when the principal kind matches (apikey)', async () => {
      const mw = SecurityMiddleware({ principalKind: 'apikey' });
      const ctx = createMockContext({ sub: 'svc-1', kind: 'apikey' });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });

    it('should return 403 when the principal kind does not match', async () => {
      const mw = SecurityMiddleware({ principalKind: 'apikey' });
      const ctx = createMockContext({ sub: 'user-1', kind: 'user' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should fail closed when the principal has no kind', async () => {
      const mw = SecurityMiddleware({ principalKind: 'user' });
      const ctx = createMockContext({ sub: 'user-1' });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should accept any of multiple allowed principal kinds', async () => {
      const mw = SecurityMiddleware({ principalKind: ['user', 'apikey'] });
      const userCtx = createMockContext({ sub: 'user-1', kind: 'user' });
      expect((await runMiddleware(mw, userCtx)).passedThrough).toBe(true);
      const keyCtx = createMockContext({ sub: 'svc-1', kind: 'apikey' });
      expect((await runMiddleware(mw, keyCtx)).passedThrough).toBe(true);
    });
  });

  describe('verify option (custom token verifier)', () => {
    const verifier = async (token: string) => {
      if (token === 'valid-token') return { sub: 'user-1', scope: 'read write' };
      return undefined;
    };

    it('should return 401 when no Authorization header', async () => {
      const mw = SecurityMiddleware({ verify: verifier });
      const ctx = createMockContext(undefined);
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(401);
    });

    it('should return 401 when token verification fails', async () => {
      const mw = SecurityMiddleware({ verify: verifier });
      const ctx = createMockContext(undefined);
      ctx.req = new Request('http://localhost/test', {
        headers: { Authorization: 'Bearer bad-token' },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(401);
    });

    it('should populate ctx.user and pass through on valid token', async () => {
      const mw = SecurityMiddleware({ verify: verifier });
      const ctx = createMockContext(undefined);
      ctx.req = new Request('http://localhost/test', {
        headers: { Authorization: 'Bearer valid-token' },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
      // A custom verifier resolves a bearer token, so the principal is stamped as
      // `kind: 'user'` (default) when the verifier does not set one — making
      // `.secure({ verify, principalKind: 'user' })` satisfiable.
      expect(ctx.user).toEqual({ sub: 'user-1', scope: 'read write', kind: 'user' });
    });

    it('should enforce scope checks after verification', async () => {
      const mw = SecurityMiddleware({ verify: verifier, scopes: ['admin'] });
      const ctx = createMockContext(undefined);
      ctx.req = new Request('http://localhost/test', {
        headers: { Authorization: 'Bearer valid-token' },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.status).toBe(403);
    });

    it('should pass scope checks after verification when scopes match', async () => {
      const mw = SecurityMiddleware({ verify: verifier, scopes: ['read'] });
      const ctx = createMockContext(undefined);
      ctx.req = new Request('http://localhost/test', {
        headers: { Authorization: 'Bearer valid-token' },
      });
      const result = await runMiddleware(mw, ctx);
      expect(result.passedThrough).toBe(true);
    });
  });
});
