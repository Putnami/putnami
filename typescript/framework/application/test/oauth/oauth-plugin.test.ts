import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { application } from '../../src/application';
import { config } from '../../src/config';
import { http } from '../../src/http/http.plugin';
import { OAuthPlugin, oAuth2 } from '../../src/oauth/oauth.plugin';

const SESSION_CONFIG = {
  session: {
    store: 'cookie',
    cookieName: 'sid',
    cookieSecret: 'a'.repeat(64),
    ttl: 300,
  },
};

// A minimal explicit provider endpoint. There are no hardcoded provider
// fallbacks, so any test that exercises the login redirect must
// configure at least the authorize endpoint.
const OAUTH_AUTHORIZE = { oauth: { authorizeUri: 'https://auth.example.com/authorize' } };

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;

function withConfig(extra: Record<string, unknown> = {}): void {
  process.env.CONFIG_DATA = JSON.stringify({ ...SESSION_CONFIG, ...extra });
  resetConfigLoader();
}

beforeEach(() => {
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

afterEach(() => {
  if (ORIGINAL_CONFIG_DATA === undefined) {
    delete process.env.CONFIG_DATA;
  } else {
    process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
  }
  resetConfigLoader();
});

describe('OAuthPlugin', () => {
  describe('oAuth2() factory', () => {
    it('should create OAuthPlugin instance', () => {
      const plugin = oAuth2();
      expect(plugin).toBeInstanceOf(OAuthPlugin);
    });

    it('should accept configuration overrides', () => {
      const plugin = oAuth2({ loginRoute: '/custom-login' });
      expect(plugin).toBeInstanceOf(OAuthPlugin);
    });
  });

  describe('Plugin Lifecycle', () => {
    it('should register login route that redirects to authorize URL', async () => {
      withConfig(OAUTH_AUTHORIZE);
      const httpPlugin = http({ port: 0 });
      const app = application().use(httpPlugin).use(config()).use(oAuth2());
      await app.start();
      const server = httpPlugin.getServer();

      const loginRes = await fetch(`http://localhost:${server?.port}/login`, { redirect: 'manual' });

      expect(loginRes.status).toBe(302);
      const location = loginRes.headers.get('location') ?? '';
      expect(location).toContain('authorize');
      expect(location).toContain('response_type=code');
      expect(location).toContain('state=');

      await app.stop();
    });

    it('should register logout route', async () => {
      withConfig();
      const httpPlugin = http({ port: 0 });
      const app = application().use(httpPlugin).use(config()).use(oAuth2());
      await app.start();
      const server = httpPlugin.getServer();

      const logoutRes = await fetch(`http://localhost:${server?.port}/logout`, { redirect: 'manual' });
      expect(logoutRes.status).not.toBe(404);

      await app.stop();
    });

    it('should register a separate callback route when configured', async () => {
      withConfig(OAUTH_AUTHORIZE);
      const httpPlugin = http({ port: 0 });
      const app = application()
        .use(httpPlugin)
        .use(config())
        .use(oAuth2({ callbackRoute: '/auth/callback' }));
      await app.start();
      const server = httpPlugin.getServer();

      // /login redirects to /authorize with the callback path encoded
      const loginRes = await fetch(`http://localhost:${server?.port}/login`, { redirect: 'manual' });
      expect(loginRes.status).toBe(302);
      expect(loginRes.headers.get('location')).toContain('%2Fauth%2Fcallback');

      // /auth/callback is registered (missing-state response is 400, not 404)
      const cb = await fetch(`http://localhost:${server?.port}/auth/callback`, { redirect: 'manual' });
      expect(cb.status).not.toBe(404);

      await app.stop();
    });
  });

  describe('Route Configuration', () => {
    it('should register routes at custom paths', async () => {
      withConfig(OAUTH_AUTHORIZE);
      const httpPlugin = http({ port: 0 });
      const app = application()
        .use(httpPlugin)
        .use(config())
        .use(oAuth2({ loginRoute: '/auth/sign-in', logoutRoute: '/auth/sign-out' }));
      await app.start();
      const server = httpPlugin.getServer();

      const loginRes = await fetch(`http://localhost:${server?.port}/auth/sign-in`, { redirect: 'manual' });
      expect(loginRes.status).toBe(302);

      const logoutRes = await fetch(`http://localhost:${server?.port}/auth/sign-out`, { redirect: 'manual' });
      expect(logoutRes.status).not.toBe(404);

      const defaultLoginRes = await fetch(`http://localhost:${server?.port}/login`);
      expect(defaultLoginRes.status).toBe(404);

      await app.stop();
    });
  });

  describe('Authorization URL', () => {
    it('should include redirect_uri pointing at the login route by default', async () => {
      withConfig(OAUTH_AUTHORIZE);
      const httpPlugin = http({ port: 0 });
      const app = application().use(httpPlugin).use(config()).use(oAuth2());
      await app.start();
      const server = httpPlugin.getServer();

      const loginRes = await fetch(`http://localhost:${server?.port}/login`, { redirect: 'manual' });

      expect(loginRes.status).toBe(302);
      const location = loginRes.headers.get('location') ?? '';
      expect(location).toContain('redirect_uri=');
      expect(location).toContain('%2Flogin');

      await app.stop();
    });

    it('should use custom authorizeUri from oAuth2() config', async () => {
      withConfig();
      const httpPlugin = http({ port: 0 });
      const app = application()
        .use(httpPlugin)
        .use(config())
        .use(oAuth2({ authorizeUri: 'https://accounts.google.com/o/oauth2/v2/auth' }));
      await app.start();
      const server = httpPlugin.getServer();

      const loginRes = await fetch(`http://localhost:${server?.port}/login`, { redirect: 'manual' });

      expect(loginRes.status).toBe(302);
      expect(loginRes.headers.get('location') ?? '').toStartWith('https://accounts.google.com/o/oauth2/v2/auth');

      await app.stop();
    });
  });
});
