import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { resetConfigLoader, runInContext, useConfig } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { HttpRequestContext } from '../../src/http';
import { OAuthConfig } from '../../src/oauth/oauth.config';
import { OAuthService, setActiveOAuthService, useOAuthService } from '../../src/oauth/oauth.service';

describe('OAuthConfig', () => {
  describe('Default Values', () => {
    it('should have default login route', () => {
      const config = useConfig(OAuthConfig);
      expect(config.loginRoute).toBe('/login');
    });

    it('should have default logout route', () => {
      const config = useConfig(OAuthConfig);
      expect(config.logoutRoute).toBe('/logout');
    });

    it('leaves endpoint URIs undefined so discovery wins by default', () => {
      const config = useConfig(OAuthConfig);
      expect(config.authorizeUri).toBeUndefined();
      expect(config.tokenUri).toBeUndefined();
      expect(config.keysUri).toBeUndefined();
      expect(config.signoutUri).toBeUndefined();
    });

    it('leaves provider endpoints undefined when neither config nor discovery is set', async () => {
      const service = new OAuthService();
      const endpoints = await service.resolveEndpoints();
      expect(endpoints.authorize).toBeUndefined();
      expect(endpoints.token).toBeUndefined();
      expect(endpoints.jwks).toBeUndefined();
      expect(endpoints.signout).toBeUndefined();
    });

    it('throws a clear config error instead of silently using a default IdP', async () => {
      const service = new OAuthService();
      await expect(service.authorizeUrl()).rejects.toThrow(/authorization endpoint is not configured/i);
    });

    it('should have default login redirect URL', () => {
      const config = useConfig(OAuthConfig);
      expect(config.defaultLoginRedirectUrl).toBe('/');
    });

    it('should have empty scopes by default', () => {
      const config = useConfig(OAuthConfig);
      expect(config.scopes).toEqual([]);
    });

    it('should have empty roles by default', () => {
      const config = useConfig(OAuthConfig);
      expect(config.roles).toEqual([]);
    });
  });

  describe('Configuration Override', () => {
    it('should allow custom configuration via confInit', () => {
      const config = useConfig(OAuthConfig, {
        confInit: {
          clientId: 'my-client-id',
          clientSecret: 'my-secret',
          authorizeUri: 'https://custom-auth.example.com/authorize',
        },
      });

      expect(config.clientId).toBe('my-client-id');
      expect(config.clientSecret).toBe('my-secret');
      expect(config.authorizeUri).toBe('https://custom-auth.example.com/authorize');
    });
  });
});

// ---------------------------------------------------------------------------
// OAuthService tests
// ---------------------------------------------------------------------------

const TOKEN_URI = 'https://auth.test/token';
const KEYS_URI = 'https://auth.test/keys';
const AUTHORIZE_URI = 'https://auth.test/authorize';
const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const originalFetch = globalThis.fetch;

function setOAuthConfig(overrides: Record<string, unknown> = {}) {
  process.env.CONFIG_DATA = JSON.stringify({
    oauth: {
      clientId: 'test-client',
      clientSecret: 'test-secret',
      tokenUri: TOKEN_URI,
      keysUri: KEYS_URI,
      authorizeUri: AUTHORIZE_URI,
      ...overrides,
    },
  });
  resetConfigLoader();
}

function createHttpContext(): HttpRequestContext {
  const req = new Request('https://app.test/callback');
  return {
    body: async () => undefined,
    domain: () => 'https://app.test',
    headers: req.headers,
    host: () => 'app.test',
    logger: new MemoryLogger(),
    method: 'GET',
    path: () => '/callback',
    query: () => '',
    queryParams: () => ({}),
    req,
    secured: () => true,
    throw: (status: number) => {
      throw new Error(`throw ${status}`);
    },
    url: req.url,
  } as HttpRequestContext;
}

describe('OAuthService', () => {
  beforeEach(() => {
    setOAuthConfig();
    setActiveOAuthService(undefined);
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (ORIGINAL_CONFIG_DATA === undefined) {
      delete process.env.CONFIG_DATA;
    } else {
      process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
    }
    resetConfigLoader();
    setActiveOAuthService(undefined);
  });

  describe('useOAuthService', () => {
    it('throws when no service is active', () => {
      expect(() => useOAuthService()).toThrow(/OAuthService is not active/);
    });

    it('returns active service', () => {
      const service = new OAuthService();
      setActiveOAuthService(service);
      expect(useOAuthService()).toBe(service);
    });
  });

  describe('clientToken', () => {
    it('fetches and returns access token', async () => {
      globalThis.fetch = mock(() =>
        Promise.resolve(new Response(JSON.stringify({ access_token: 'tok-123', expires_in: 3600 }), { status: 200 })),
      ) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { clientId: 'test-client', clientSecret: 'test-secret', tokenUri: TOKEN_URI };
      const token = await service.clientToken();
      expect(token).toBe('tok-123');
    });

    it('caches token on second call', async () => {
      let callCount = 0;
      globalThis.fetch = mock(() => {
        callCount++;
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: 'cached', expires_in: 3600 }), { status: 200 }),
        );
      }) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { clientId: 'c', clientSecret: 's', tokenUri: TOKEN_URI };
      await service.clientToken();
      await service.clientToken();
      expect(callCount).toBe(1);
    });

    it('returns undefined when token request fails', async () => {
      globalThis.fetch = mock(() => Promise.resolve(new Response('error', { status: 401 }))) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { clientId: 'c', clientSecret: 's', tokenUri: TOKEN_URI };
      const context = createHttpContext();
      const token = await runInContext(context, () => service.clientToken());
      expect(token).toBeUndefined();
    });
  });

  describe('refreshToken', () => {
    it('exchanges refresh token for new tokens', async () => {
      globalThis.fetch = mock(() =>
        Promise.resolve(
          new Response(JSON.stringify({ access_token: 'new-tok', refresh_token: 'new-ref', expires_in: 3600 }), {
            status: 200,
          }),
        ),
      ) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { clientId: 'c', clientSecret: 's', tokenUri: TOKEN_URI };
      const result = await service.refreshToken('old-refresh');
      expect(result?.access_token).toBe('new-tok');
    });
  });

  describe('authorizeUrl', () => {
    it('builds authorization URL with params', async () => {
      const service = new OAuthService();
      service.confInit = { clientId: 'my-app', authorizeUri: AUTHORIZE_URI };

      const context = createHttpContext();
      const url = await runInContext(context, () => service.authorizeUrl());
      expect(url).toContain(AUTHORIZE_URI);
      expect(url).toContain('client_id=');
      expect(url).toContain('response_type=code');
    });
  });

  describe('verify', () => {
    it('returns undefined when keys fetch fails', async () => {
      globalThis.fetch = mock(() => Promise.resolve(new Response('', { status: 500 }))) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { keysUri: KEYS_URI };

      const context = createHttpContext();
      const result = await runInContext(context, () => service.verify('some.jwt.token'));
      expect(result).toBeUndefined();
    });
  });

  describe('public key cache TTL', () => {
    it('re-fetches keys after TTL expires', async () => {
      let fetchCount = 0;
      globalThis.fetch = mock(() => {
        fetchCount++;
        return Promise.resolve(
          new Response(JSON.stringify({ keys: [{ kty: 'RSA', n: 'AQAB', e: 'AQAB', use: 'sig' }] }), {
            status: 200,
          }),
        );
      }) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { keysUri: KEYS_URI };

      const context = createHttpContext();

      // First call fetches keys
      await runInContext(context, () => service.verify('some.jwt.token'));
      expect(fetchCount).toBe(1);

      // Second call uses cache
      await runInContext(context, () => service.verify('some.jwt.token'));
      expect(fetchCount).toBe(1);

      // Expire the cache by manipulating the internal state
      const cacheField = '_publicKeys' as keyof OAuthService;
      const cache = (service as Record<string, unknown>)[cacheField] as { keys: string[]; cachedAt: number };
      cache.cachedAt = Date.now() - 61 * 60 * 1000; // 61 minutes ago

      // Third call re-fetches
      await runInContext(context, () => service.verify('some.jwt.token'));
      expect(fetchCount).toBe(2);
    });
  });

  describe('fetchToken error handling', () => {
    it('logs error on non-200 response', async () => {
      globalThis.fetch = mock(() => Promise.resolve(new Response('Unauthorized', { status: 401 }))) as typeof fetch;

      const service = new OAuthService();
      service.confInit = { clientId: 'c', clientSecret: 's', tokenUri: TOKEN_URI };
      const logger = new MemoryLogger();
      const context = createHttpContext();
      (context as Record<string, unknown>).logger = logger;

      const result = await runInContext(context, () => service.clientToken());
      expect(result).toBeUndefined();
      expect(logger.entries.some((e) => e.level === 'error')).toBe(true);
    });
  });
});
