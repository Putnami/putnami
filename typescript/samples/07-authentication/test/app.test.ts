import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { HttpPlugin, api, oAuth2, platform, redirect } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';

describe('authentication sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [
        platform(),
        oAuth2({
          authorizeUri: 'https://accounts.google.com/o/oauth2/v2/auth',
          tokenUri: 'https://oauth2.googleapis.com/token',
          clientId: 'test-client-id',
          clientSecret: 'test-client-secret',
          scopes: ['openid', 'email', 'profile'],
        }),
        api({ scanPath: 'src/api' }),
      ],
      configure: (app) => {
        app.getPlugin(HttpPlugin).get('/', () => redirect('/profile'));
      },
    });
  });

  afterAll(async () => {
    await testApp.stop();
  });

  describe('GET /healthz', () => {
    it('should return healthy status', async () => {
      const res = await testApp.fetch('/healthz');
      expect(res.status).toBe(200);
    });
  });

  describe('GET /login', () => {
    it('should redirect to OAuth authorize URL', async () => {
      const res = await testApp.fetch('/login', { redirect: 'manual' });
      expect(res.status).toBe(302);

      const authorizeUrl = new URL(res.headers.get('location') ?? '');
      expect(`${authorizeUrl.origin}${authorizeUrl.pathname}`).toBe('https://accounts.google.com/o/oauth2/v2/auth');
      expect(authorizeUrl.searchParams.get('response_type')).toBe('code');
      expect(authorizeUrl.searchParams.get('client_id')).toBe('test-client-id');
    });
  });

  describe('GET /auth/session', () => {
    it('should return unauthenticated when no session', async () => {
      const res = await testApp.fetch('/auth/session');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.authenticated).toBe(false);
      expect(data.message).toContain('Not logged in');
    });
  });

  describe('GET /profile', () => {
    it('should return 401 when not authenticated', async () => {
      const res = await testApp.fetch('/profile');
      expect(res.status).toBe(401);
    });
  });

  describe('GET /protected', () => {
    it('should return 401 when not authenticated', async () => {
      const res = await testApp.fetch('/protected');
      expect(res.status).toBe(401);
    });
  });

  describe('GET /', () => {
    it('should redirect to /profile', async () => {
      const res = await testApp.fetch('/', { redirect: 'manual' });
      expect(res.status).toBe(302);
      expect(res.headers.get('location')).toContain('/profile');
    });
  });
});
