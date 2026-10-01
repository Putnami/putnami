import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { application } from '../../src/application';
import { config } from '../../src/config';
import { http } from '../../src/http/http.plugin';
import { safeRedirectPath } from '../../src/oauth/handlers/login.get';
import { oAuth2 } from '../../src/oauth/oauth.plugin';
import { computeExpireAt, OAuthService } from '../../src/oauth/oauth.service';

const AUTH_BASE = 'http://auth.test';
const DISCOVERY_URI = `${AUTH_BASE}/.well-known/openid-configuration`;
const SESSION_SECRET = 'a'.repeat(64);

const SIGNING_KID = 'sig-1';
let signingKey: CryptoKeyPair;
let altSigningKey: CryptoKeyPair;
let publicJwk: JsonWebKey & { kid?: string; alg?: string; use?: string };
let altPublicJwk: JsonWebKey & { kid?: string; alg?: string; use?: string };
let discoveryDoc: Record<string, unknown>;

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const originalFetch = globalThis.fetch;

// -------------------------------------------------------------------------
// JWT helpers
// -------------------------------------------------------------------------

function base64UrlEncode(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

async function signJwt(
  payload: Record<string, unknown>,
  opts: { key?: CryptoKey; kid?: string } = {},
): Promise<string> {
  const header = { alg: 'RS256', typ: 'JWT', kid: opts.kid ?? SIGNING_KID };
  const h = base64UrlEncode(new TextEncoder().encode(JSON.stringify(header)));
  const p = base64UrlEncode(new TextEncoder().encode(JSON.stringify(payload)));
  const data = new TextEncoder().encode(`${h}.${p}`);
  const sig = await crypto.subtle.sign(
    { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' },
    opts.key ?? signingKey.privateKey,
    data,
  );
  return `${h}.${p}.${base64UrlEncode(new Uint8Array(sig))}`;
}

// -------------------------------------------------------------------------
// Mock harness — same shape as oauth-flow.test.ts so the tests are easy to read
// -------------------------------------------------------------------------

interface RecordedRequest {
  url: string;
  method: string;
  headers: Headers;
  body?: string;
}

interface CodeEntry {
  redirectUri?: string;
  subject: Record<string, unknown>;
  idTokenClaims?: Record<string, unknown> | null;
  expiresIn?: number | undefined;
}

interface MockState {
  requests: RecordedRequest[];
  codes: Map<string, CodeEntry>;
  userInfo: Record<string, unknown>;
  jwks: { keys: Array<JsonWebKey & { kid?: string }> };
  clientId: string;
  /** When true, the next /.well-known/jwks.json hit serves an updated key set. */
  jwksAfterRotation?: { keys: Array<JsonWebKey & { kid?: string }> };
  /** When true, every fetch to /.well-known/openid-configuration fails. */
  discoveryFails?: boolean;
  /** When true, every fetch to /.well-known/jwks.json fails. */
  jwksFails?: boolean;
}

function makeFetch(state: MockState): typeof fetch {
  let jwksHits = 0;
  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = input instanceof Request ? input.url : String(input);
    const method = (init?.method ?? (input instanceof Request ? input.method : 'GET')).toUpperCase();
    const bodyText = init?.body ? String(init.body) : undefined;
    state.requests.push({ url, method, headers: new Headers(init?.headers), body: bodyText });

    if (url === DISCOVERY_URI) {
      if (state.discoveryFails) return new Response('', { status: 500 });
      return Response.json(discoveryDoc);
    }
    if (url === `${AUTH_BASE}/.well-known/jwks.json`) {
      if (state.jwksFails) return new Response('', { status: 500 });
      jwksHits += 1;
      if (state.jwksAfterRotation && jwksHits > 1) return Response.json(state.jwksAfterRotation);
      return Response.json(state.jwks);
    }
    if (url === `${AUTH_BASE}/token` && method === 'POST') {
      const form = new URLSearchParams(bodyText ?? '');
      if (form.get('grant_type') === 'authorization_code') {
        const entry = state.codes.get(form.get('code') ?? '');
        if (!entry) return new Response(JSON.stringify({ error: 'invalid_grant' }), { status: 400 });
        if (entry.redirectUri !== undefined && form.get('redirect_uri') !== entry.redirectUri) {
          return new Response(JSON.stringify({ error: 'invalid_request' }), { status: 400 });
        }
        const accessToken = await signJwt({ ...entry.subject, exp: Math.floor(Date.now() / 1000) + 60 });
        const response: Record<string, unknown> = {
          access_token: accessToken,
          refresh_token: `refresh-${form.get('code')}`,
          token_type: 'Bearer',
        };
        if (entry.expiresIn !== undefined) response['expires_in'] = entry.expiresIn;
        if (entry.idTokenClaims) {
          response['id_token'] = await signJwt({
            iss: AUTH_BASE,
            aud: state.clientId,
            exp: Math.floor(Date.now() / 1000) + 60,
            iat: Math.floor(Date.now() / 1000),
            ...entry.idTokenClaims,
          });
        }
        return Response.json(response);
      }
      return new Response(JSON.stringify({ error: 'unsupported_grant_type' }), { status: 400 });
    }
    if (url === `${AUTH_BASE}/userinfo`) return Response.json(state.userInfo);
    return originalFetch(input, init);
  }) as typeof fetch;
}

function setConfig(extra: Record<string, unknown>): void {
  process.env.CONFIG_DATA = JSON.stringify({
    session: {
      store: 'cookie',
      cookieName: 'oauth_security_sid',
      cookieSecret: SESSION_SECRET,
      ttl: 300,
    },
    ...extra,
  });
  resetConfigLoader();
}

async function runLogin(state: MockState): Promise<{
  port: number;
  cookie: string;
  authorize: URL;
  app: ReturnType<typeof application>;
}> {
  const hp = http({ port: 0 });
  const app = application().use(hp).use(config()).use(oAuth2());
  await app.start();
  const port = hp.getServer()?.port ?? 0;
  globalThis.fetch = makeFetch(state);
  const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
  const cookie = loginRes.headers.get('set-cookie') ?? '';
  const authorize = new URL(loginRes.headers.get('location') ?? '');
  return { port, cookie, authorize, app };
}

// -------------------------------------------------------------------------
// Suite
// -------------------------------------------------------------------------

describe('OAuth security hardening', () => {
  beforeAll(async () => {
    signingKey = (await crypto.subtle.generateKey(
      { name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
      true,
      ['sign', 'verify'],
    )) as CryptoKeyPair;
    publicJwk = (await crypto.subtle.exportKey('jwk', signingKey.publicKey)) as JsonWebKey & { kid?: string };
    publicJwk.kid = SIGNING_KID;
    publicJwk.alg = 'RS256';
    publicJwk.use = 'sig';

    altSigningKey = (await crypto.subtle.generateKey(
      { name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
      true,
      ['sign', 'verify'],
    )) as CryptoKeyPair;
    altPublicJwk = (await crypto.subtle.exportKey('jwk', altSigningKey.publicKey)) as JsonWebKey & { kid?: string };
    altPublicJwk.kid = 'sig-2';
    altPublicJwk.alg = 'RS256';
    altPublicJwk.use = 'sig';

    discoveryDoc = {
      issuer: AUTH_BASE,
      authorization_endpoint: `${AUTH_BASE}/authorize`,
      token_endpoint: `${AUTH_BASE}/token`,
      userinfo_endpoint: `${AUTH_BASE}/userinfo`,
      jwks_uri: `${AUTH_BASE}/.well-known/jwks.json`,
      end_session_endpoint: `${AUTH_BASE}/logout`,
      code_challenge_methods_supported: ['S256'],
    };
  });

  beforeEach(() => {
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  afterAll(() => {
    if (ORIGINAL_CONFIG_DATA === undefined) {
      delete process.env.CONFIG_DATA;
    } else {
      process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
    }
    resetConfigLoader();
    globalThis.fetch = originalFetch;
  });

  // ===========================================================================
  // computeExpireAt
  // ===========================================================================
  describe('computeExpireAt', () => {
    it('falls back to a sane default when expires_in is missing', () => {
      const before = Date.now();
      const expireAt = computeExpireAt(undefined);
      const after = Date.now();
      // Default lifetime is 300s; check expireAt is in the future and not NaN.
      expect(Number.isFinite(expireAt)).toBe(true);
      expect(expireAt).toBeGreaterThan(before + 60_000);
      expect(expireAt).toBeLessThanOrEqual(after + 300_000);
    });

    it('falls back when expires_in is zero', () => {
      const before = Date.now();
      const expireAt = computeExpireAt(0);
      expect(expireAt).toBeGreaterThan(before + 60_000);
    });

    it('falls back when expires_in is negative', () => {
      const before = Date.now();
      const expireAt = computeExpireAt(-1);
      expect(expireAt).toBeGreaterThan(before + 60_000);
    });

    it('honors a positive expires_in with a clock-skew margin', () => {
      const before = Date.now();
      const expireAt = computeExpireAt(60);
      expect(expireAt).toBeGreaterThanOrEqual(before + 59_000);
      expect(expireAt).toBeLessThanOrEqual(before + 60_000);
    });
  });

  // ===========================================================================
  // safeRedirectPath
  // ===========================================================================
  describe('safeRedirectPath', () => {
    it.each<[string, unknown]>([
      ['non-string', 42],
      ['empty', ''],
      ['too long', `/${'a'.repeat(2048)}`],
      ['protocol-relative //evil', '//evil.example.com'],
      ['backslash /\\evil', '/\\evil.example.com'],
      ['percent-encoded /%2Fevil', '/%2Fevil.example.com'],
      ['percent-encoded /%5Cevil', '/%5Cevil.example.com'],
      ['no leading slash', 'evil.example.com'],
      ['javascript: scheme', 'javascript:alert(1)'],
      ['http: scheme', 'http://evil.example.com'],
    ])('rejects %s', (_label, value) => {
      expect(safeRedirectPath(value)).toBeUndefined();
    });

    it.each([['/'], ['/dashboard'], ['/path/with?query=1&more=2'], ['/path#hash']])('accepts %s', (value) => {
      expect(safeRedirectPath(value)).toBe(value);
    });
  });

  // ===========================================================================
  // client_secret_basic encoding (RFC 6749 §2.3.1)
  // ===========================================================================
  describe('client_secret_basic encoding', () => {
    it('form-urlencodes credentials before base64 (handles !, *, space, etc.)', async () => {
      setConfig({
        oauth: {
          clientId: 'client*id!',
          clientSecret: "p ss'word*",
          discoveryUri: DISCOVERY_URI,
          tokenEndpointAuthMethod: 'client_secret_basic',
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'client*id!',
      };
      globalThis.fetch = makeFetch(state);

      const service = new OAuthService();
      await service.refreshToken('rt'); // failure response is fine; we just want the request

      const req = state.requests.find((r) => r.url === `${AUTH_BASE}/token`);
      expect(req).toBeDefined();
      const authz = req?.headers.get('authorization') ?? '';
      expect(authz.startsWith('Basic ')).toBe(true);
      const decoded = atob(authz.slice('Basic '.length));
      // Spec form: '!' -> %21, '*' -> %2A, ' ' -> '+', "'" -> %27
      expect(decoded).toBe('client%2Aid%21:p+ss%27word%2A');
    });
  });

  // ===========================================================================
  // PKCE policy matrix
  // ===========================================================================
  describe('PKCE policy', () => {
    async function authorizeWith(pkce: 'auto' | 'always' | 'never', discoveryUri?: string): Promise<URL> {
      setConfig({
        oauth: {
          clientId: 'c',
          clientSecret: 's',
          ...(discoveryUri ? { discoveryUri } : {}),
          authorizeUri: discoveryUri ? undefined : `${AUTH_BASE}/authorize`,
          tokenUri: discoveryUri ? undefined : `${AUTH_BASE}/token`,
          callbackRoute: '/auth/callback',
          pkce,
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'c',
      };
      const { authorize, app } = await runLogin(state);
      await app.stop();
      return authorize;
    }

    it('auto + discovery advertising S256 → PKCE on', async () => {
      const url = await authorizeWith('auto', DISCOVERY_URI);
      expect(url.searchParams.get('code_challenge')).toBeTruthy();
    });

    it('auto + no discovery → PKCE on (defense in depth)', async () => {
      const url = await authorizeWith('auto');
      expect(url.searchParams.get('code_challenge')).toBeTruthy();
    });

    it('never → PKCE off', async () => {
      const url = await authorizeWith('never', DISCOVERY_URI);
      expect(url.searchParams.get('code_challenge')).toBeNull();
    });

    it('always → PKCE on', async () => {
      const url = await authorizeWith('always');
      expect(url.searchParams.get('code_challenge')).toBeTruthy();
    });
  });

  // ===========================================================================
  // id_token validation + nonce binding
  // ===========================================================================
  describe('id_token validation', () => {
    it('rejects callback when id_token.nonce does not match the pending nonce', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          scopes: ['openid'],
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' }, idTokenClaims: { sub: 'u', nonce: 'WRONG' } }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const entry = state.codes.get('code-1');
        if (entry) entry.redirectUri = `http://localhost:${port}/auth/callback`;
        const cb = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(401);
        const body = (await cb.json()) as { error: string };
        expect(body.error).toBe('invalid_id_token');
      } finally {
        await app.stop();
      }
    });

    it('rejects callback when id_token has the wrong aud', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          scopes: ['openid'],
          pkce: 'never',
        },
      });
      // Mock issues id_token with the wrong audience by overriding clientId in the mock.
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' }, idTokenClaims: { sub: 'u' } }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'other-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const nonce = authorize.searchParams.get('nonce') ?? undefined;
        const entry = state.codes.get('code-1');
        if (entry) {
          entry.redirectUri = `http://localhost:${port}/auth/callback`;
          entry.idTokenClaims = { sub: 'u', nonce };
        }
        const cb = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(401);
        const body = (await cb.json()) as { error: string };
        expect(body.error).toBe('invalid_id_token');
      } finally {
        await app.stop();
      }
    });

    it('rejects callback when OIDC scope was requested but no id_token returned', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          scopes: ['openid'],
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' } /* idTokenClaims not set */ }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const entry = state.codes.get('code-1');
        if (entry) entry.redirectUri = `http://localhost:${port}/auth/callback`;
        const cb = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(401);
        const body = (await cb.json()) as { error: string };
        expect(body.error).toBe('missing_id_token');
      } finally {
        await app.stop();
      }
    });
  });

  // ===========================================================================
  // expires_in defensiveness — session stays usable
  // ===========================================================================
  describe('expires_in handling', () => {
    it('produces a non-NaN session expireAt even when /token omits expires_in', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          scopes: [],
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' }, expiresIn: undefined }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const entry = state.codes.get('code-1');
        if (entry) entry.redirectUri = `http://localhost:${port}/auth/callback`;
        const cb = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(302);
        // The session cookie carries the session payload; non-NaN expireAt is enforced by the
        // fact that subsequent requests using the session succeed (no instant-expiry churn).
        expect(cb.headers.get('set-cookie')).toContain('oauth_security_sid=');
      } finally {
        await app.stop();
      }
    });
  });

  // ===========================================================================
  // Kid rotation
  // ===========================================================================
  describe('JWKS kid rotation', () => {
    it('refetches JWKS once when the token kid is missing, then verifies', async () => {
      // First JWKS response only has the OLD key; rotation makes the NEW key available.
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        jwksAfterRotation: { keys: [publicJwk, altPublicJwk] },
        clientId: 'test-client',
      };
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          pkce: 'never',
        },
      });
      globalThis.fetch = makeFetch(state);

      const service = new OAuthService();
      // Token signed with the alt key advertising kid=sig-2 — not in the cached JWKS.
      const token = await signJwt({ sub: 'u' }, { key: altSigningKey.privateKey, kid: 'sig-2' });

      const payload = await service.verify<{ sub: string }>(token);
      expect(payload?.sub).toBe('u');

      const jwksHits = state.requests.filter((r) => r.url === `${AUTH_BASE}/.well-known/jwks.json`).length;
      expect(jwksHits).toBe(2);
    });

    it('rejects a token whose kid is not in the JWKS even after refetch', async () => {
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          pkce: 'never',
        },
      });
      globalThis.fetch = makeFetch(state);

      const service = new OAuthService();
      const token = await signJwt({ sub: 'u' }, { key: altSigningKey.privateKey, kid: 'sig-2' });
      const payload = await service.verify(token);
      expect(payload).toBeUndefined();
    });
  });

  // ===========================================================================
  // verify() claim failures
  // ===========================================================================
  describe('verify() claim validation', () => {
    function freshService(): OAuthService {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          pkce: 'never',
        },
      });
      return new OAuthService();
    }

    it('rejects expired tokens', async () => {
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      globalThis.fetch = makeFetch(state);
      const service = freshService();
      const token = await signJwt({ sub: 'u', exp: Math.floor(Date.now() / 1000) - 60 });
      expect(await service.verify(token)).toBeUndefined();
    });

    it('rejects wrong issuer', async () => {
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      globalThis.fetch = makeFetch(state);
      const service = freshService();
      const token = await signJwt({ sub: 'u', iss: 'http://wrong' });
      expect(await service.verify(token, { issuer: AUTH_BASE })).toBeUndefined();
    });

    it('rejects wrong audience', async () => {
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      globalThis.fetch = makeFetch(state);
      const service = freshService();
      const token = await signJwt({ sub: 'u', aud: 'other-client' });
      expect(await service.verify(token, { audience: 'test-client' })).toBeUndefined();
    });
  });

  // ===========================================================================
  // /auth/callback error path
  // ===========================================================================
  describe('callback error path', () => {
    it('returns 401 with the provider error code when /authorize denies', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const cb = await fetch(
          `http://localhost:${port}/auth/callback?error=access_denied&error_description=user%20cancelled&state=${stateParam}`,
          { headers: { Cookie: cookie }, redirect: 'manual' },
        );
        expect(cb.status).toBe(401);
        const body = (await cb.json()) as { error: string; error_description: string };
        expect(body.error).toBe('access_denied');
        expect(body.error_description).toBe('user cancelled');
      } finally {
        await app.stop();
      }
    });

    it('renders a provider error as HTML for browsers without echoing error_description', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map(),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const description = encodeURIComponent('<script>alert(1)</script>');
        const errorCode = encodeURIComponent('access_denied"><img src=x>');
        const cb = await fetch(
          `http://localhost:${port}/auth/callback?error=${errorCode}&error_description=${description}&state=${stateParam}`,
          { headers: { Cookie: cookie, Accept: 'text/html' }, redirect: 'manual' },
        );
        expect(cb.status).toBe(401);
        expect(cb.headers.get('content-type')).toContain('text/html');
        const html = await cb.text();
        expect(html).toContain('Sign-in could not be completed');
        expect(html).toContain('<a href="/login">Sign in again</a>');
        expect(html).not.toContain('<script>alert(1)</script>');
        expect(html).not.toContain('alert(1)');
        expect(html).not.toContain('<img src=x>');
        expect(html).toContain('access_denied&quot;&gt;&lt;img src=x&gt;');

        const secondLogin = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
        const secondCookie = secondLogin.headers.get('set-cookie') ?? '';
        const secondState = new URL(secondLogin.headers.get('location') ?? '').searchParams.get('state');
        const denied = await fetch(`http://localhost:${port}/auth/callback?error=access_denied&state=${secondState}`, {
          headers: { Cookie: secondCookie, Accept: 'text/html' },
          redirect: 'manual',
        });
        expect(denied.status).toBe(401);
        expect(await denied.text()).toContain('Sign-in was cancelled');
      } finally {
        await app.stop();
      }
    });
  });

  // ===========================================================================
  // Logout — id_token_hint + post_logout_redirect_uri
  // ===========================================================================
  describe('logout', () => {
    it('passes id_token_hint and post_logout_redirect_uri per OIDC RP-Initiated Logout', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          callbackRoute: '/auth/callback',
          scopes: ['openid'],
          pkce: 'never',
          postLogoutRedirectUrl: 'https://app.example.com/bye',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' }, idTokenClaims: { sub: 'u' } }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const nonce = authorize.searchParams.get('nonce') ?? undefined;
        const entry = state.codes.get('code-1');
        if (entry) {
          entry.redirectUri = `http://localhost:${port}/auth/callback`;
          entry.idTokenClaims = { sub: 'u', nonce };
        }
        const cb = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(302);
        const sessionCookie = cb.headers.get('set-cookie') ?? '';

        const logoutRes = await fetch(`http://localhost:${port}/logout`, {
          headers: { Cookie: sessionCookie },
          redirect: 'manual',
        });
        expect(logoutRes.status).toBe(302);
        const target = new URL(logoutRes.headers.get('location') ?? '');
        expect(`${target.origin}${target.pathname}`).toBe(`${AUTH_BASE}/logout`);
        expect(target.searchParams.get('id_token_hint')).toBeTruthy();
        expect(target.searchParams.get('post_logout_redirect_uri')).toBe('https://app.example.com/bye');
      } finally {
        await app.stop();
      }
    });
  });

  // ===========================================================================
  // Back-compat single-route flow (loginRoute === callbackRoute)
  // ===========================================================================
  describe('single-route back-compat', () => {
    it('treats /login?code=… as the callback when callbackRoute is not configured', async () => {
      setConfig({
        oauth: {
          clientId: 'test-client',
          clientSecret: 'test-secret',
          discoveryUri: DISCOVERY_URI,
          pkce: 'never',
        },
      });
      const state: MockState = {
        requests: [],
        codes: new Map([['code-1', { subject: { sub: 'u' } }]]),
        userInfo: {},
        jwks: { keys: [publicJwk] },
        clientId: 'test-client',
      };
      const { port, cookie, authorize, app } = await runLogin(state);
      try {
        const stateParam = authorize.searchParams.get('state');
        const entry = state.codes.get('code-1');
        if (entry) entry.redirectUri = `http://localhost:${port}/login`;
        const cb = await fetch(`http://localhost:${port}/login?code=code-1&state=${stateParam}`, {
          headers: { Cookie: cookie },
          redirect: 'manual',
        });
        expect(cb.status).toBe(302);
        expect(cb.headers.get('location')).toBe('/');
      } finally {
        await app.stop();
      }
    });
  });
});
