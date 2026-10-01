import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { application } from '../../src/application';
import { config } from '../../src/config';
import { http } from '../../src/http/http.plugin';
import { oAuth2 } from '../../src/oauth/oauth.plugin';
import { OAuthService } from '../../src/oauth/oauth.service';

const AUTH_BASE = 'http://auth.test';
const DISCOVERY_URI = `${AUTH_BASE}/.well-known/openid-configuration`;
const SESSION_SECRET = 'a'.repeat(64);

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const originalFetch = globalThis.fetch;

const SIGNING_KID = 'sig-1';
let signingKey: CryptoKeyPair;
let publicJwk: JsonWebKey & { kid?: string; alg?: string; use?: string };
let discoveryDoc: Record<string, unknown>;

// -------------------------------------------------------------------------
// JWT helpers
// -------------------------------------------------------------------------

function base64UrlEncode(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64UrlEncodeString(value: string): string {
  return base64UrlEncode(new TextEncoder().encode(value));
}

async function signJwt(payload: Record<string, unknown>): Promise<string> {
  const header = { alg: 'RS256', typ: 'JWT', kid: SIGNING_KID };
  const headerSegment = base64UrlEncodeString(JSON.stringify(header));
  const payloadSegment = base64UrlEncodeString(JSON.stringify(payload));
  const data = new TextEncoder().encode(`${headerSegment}.${payloadSegment}`);
  const sig = await crypto.subtle.sign({ name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' }, signingKey.privateKey, data);
  return `${headerSegment}.${payloadSegment}.${base64UrlEncode(new Uint8Array(sig))}`;
}

// -------------------------------------------------------------------------
// fetch interception
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
  opaque?: boolean;
  /** When set, the mock issues an id_token with these claims (plus `iss`/`aud`/`exp`). */
  idTokenClaims?: Record<string, unknown>;
}

interface MockState {
  requests: RecordedRequest[];
  codes: Map<string, CodeEntry>;
  refreshTokens: Map<string, Record<string, unknown>>;
  userInfo: Record<string, unknown>;
  jwks: { keys: Array<JsonWebKey & { kid?: string }> };
  /** Audience used when issuing id_tokens. Defaults to test client id. */
  clientId?: string;
}

function makeFetch(state: MockState): typeof fetch {
  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = input instanceof Request ? input.url : String(input);
    const method = (init?.method ?? (input instanceof Request ? input.method : 'GET')).toUpperCase();
    const bodyText = init?.body ? String(init.body) : undefined;
    state.requests.push({ url, method, headers: new Headers(init?.headers), body: bodyText });

    if (url === DISCOVERY_URI) return Response.json(discoveryDoc);
    if (url === `${AUTH_BASE}/.well-known/jwks.json`) return Response.json(state.jwks);

    if (url === `${AUTH_BASE}/token` && method === 'POST') {
      const form = new URLSearchParams(bodyText ?? '');
      const grant = form.get('grant_type');
      if (grant === 'authorization_code') {
        const entry = state.codes.get(form.get('code') ?? '');
        if (!entry) return new Response(JSON.stringify({ error: 'invalid_grant' }), { status: 400 });
        if (entry.redirectUri !== undefined && form.get('redirect_uri') !== entry.redirectUri) {
          return new Response(JSON.stringify({ error: 'invalid_request' }), { status: 400 });
        }
        const accessToken = entry.opaque
          ? 'opaque-token'
          : await signJwt({ ...entry.subject, exp: Math.floor(Date.now() / 1000) + 60 });
        const rt = `refresh-${form.get('code')}`;
        state.refreshTokens.set(rt, entry.subject);
        const response: Record<string, unknown> = {
          access_token: accessToken,
          refresh_token: rt,
          token_type: 'Bearer',
          expires_in: 60,
        };
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
      if (grant === 'refresh_token') {
        const subject = state.refreshTokens.get(form.get('refresh_token') ?? '');
        if (!subject) return new Response(JSON.stringify({ error: 'invalid_grant' }), { status: 400 });
        const accessToken = await signJwt({ ...subject, exp: Math.floor(Date.now() / 1000) + 60 });
        return Response.json({
          access_token: accessToken,
          refresh_token: 'rotated-refresh',
          token_type: 'Bearer',
          expires_in: 60,
        });
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
      cookieName: 'oauth_flow_sid',
      cookieSecret: SESSION_SECRET,
      ttl: 300,
    },
    ...extra,
  });
  resetConfigLoader();
}

// -------------------------------------------------------------------------
// Suite
// -------------------------------------------------------------------------

describe('OAuth2 / OIDC end-to-end flow', () => {
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

    discoveryDoc = {
      issuer: AUTH_BASE,
      authorization_endpoint: `${AUTH_BASE}/authorize`,
      token_endpoint: `${AUTH_BASE}/token`,
      userinfo_endpoint: `${AUTH_BASE}/userinfo`,
      jwks_uri: `${AUTH_BASE}/.well-known/jwks.json`,
      end_session_endpoint: `${AUTH_BASE}/logout`,
      code_challenge_methods_supported: ['S256'],
      token_endpoint_auth_methods_supported: ['client_secret_post'],
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
  });

  // -----------------------------------------------------------------------

  it('sends client_id, redirect_uri, response_type, scope, state to /authorize', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        scopes: ['openid', 'profile', 'email'],
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map(),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const res = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      expect(res.status).toBe(302);
      const url = new URL(res.headers.get('location') ?? '');

      expect(`${url.origin}${url.pathname}`).toBe(`${AUTH_BASE}/authorize`);
      expect(url.searchParams.get('response_type')).toBe('code');
      expect(url.searchParams.get('client_id')).toBe('test-client');
      expect(url.searchParams.get('redirect_uri')).toBe(`http://localhost:${port}/auth/callback`);
      expect(url.searchParams.get('scope')).toBe('openid profile email');
      expect(url.searchParams.get('state')).toBeTruthy();
      expect(url.searchParams.get('nonce')).toBeTruthy();
      expect(url.searchParams.get('code_challenge')).toBeTruthy();
      expect(url.searchParams.get('code_challenge_method')).toBe('S256');
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('sends grant_type, code, client_id, client_secret, redirect_uri to /token', async () => {
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
      codes: new Map([['code-1', { redirectUri: undefined, subject: { sub: 'user-1', email: 'a@b.c' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
      clientId: 'test-client',
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      const cookie = loginRes.headers.get('set-cookie') ?? '';
      const authorizeUrl = new URL(loginRes.headers.get('location') ?? '');
      const stateParam = authorizeUrl.searchParams.get('state');
      const nonce = authorizeUrl.searchParams.get('nonce') ?? undefined;
      const code = state.codes.get('code-1');
      if (code) {
        code.redirectUri = `http://localhost:${port}/auth/callback`;
        code.idTokenClaims = { sub: 'user-1', nonce };
      }

      const cbRes = await fetch(`http://localhost:${port}/auth/callback?code=code-1&state=${stateParam}`, {
        headers: { Cookie: cookie },
        redirect: 'manual',
      });
      expect(cbRes.status).toBe(302);

      const tokenReq = state.requests.find((r) => r.url === `${AUTH_BASE}/token`);
      expect(tokenReq).toBeDefined();
      expect(tokenReq?.headers.get('content-type')).toBe('application/x-www-form-urlencoded');
      const body = new URLSearchParams(tokenReq?.body ?? '');
      expect(body.get('grant_type')).toBe('authorization_code');
      expect(body.get('code')).toBe('code-1');
      expect(body.get('client_id')).toBe('test-client');
      expect(body.get('client_secret')).toBe('test-secret');
      expect(body.get('redirect_uri')).toBe(`http://localhost:${port}/auth/callback`);
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('sends code_verifier on the token request when PKCE is in use', async () => {
    setConfig({
      oauth: {
        clientId: 'public-client',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        scopes: ['openid'],
        tokenEndpointAuthMethod: 'none',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map([['code-2', { redirectUri: undefined, subject: { sub: 'user-2' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      const cookie = loginRes.headers.get('set-cookie') ?? '';
      const authorize = new URL(loginRes.headers.get('location') ?? '');
      const stateParam = authorize.searchParams.get('state');
      const challenge = authorize.searchParams.get('code_challenge');
      expect(challenge).toBeTruthy();
      state.codes.get('code-2')!.redirectUri = `http://localhost:${port}/auth/callback`;

      await fetch(`http://localhost:${port}/auth/callback?code=code-2&state=${stateParam}`, {
        headers: { Cookie: cookie },
        redirect: 'manual',
      });

      const tokenReq = state.requests.find((r) => r.url === `${AUTH_BASE}/token`);
      expect(tokenReq).toBeDefined();
      const body = new URLSearchParams(tokenReq?.body ?? '');
      expect(body.get('code_verifier')).toBeTruthy();
      expect(body.get('client_secret')).toBeNull();
      const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(body.get('code_verifier')!));
      expect(base64UrlEncode(new Uint8Array(digest))).toBe(challenge);
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('uses Basic auth when tokenEndpointAuthMethod is client_secret_basic', async () => {
    setConfig({
      oauth: {
        clientId: 'client-id',
        clientSecret: 'client-secret',
        discoveryUri: DISCOVERY_URI,
        tokenEndpointAuthMethod: 'client_secret_basic',
        pkce: 'never',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map(),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const service = new OAuthService();
    await service.refreshToken('any-refresh');
    const req = state.requests.find((r) => r.url === `${AUTH_BASE}/token`);
    expect(req).toBeDefined();
    expect(req?.headers.get('authorization')).toBe(`Basic ${btoa('client-id:client-secret')}`);
    const body = new URLSearchParams(req?.body ?? '');
    expect(body.get('client_secret')).toBeNull();
    expect(body.get('client_id')).toBe('client-id');
  });

  // -----------------------------------------------------------------------

  it('refreshes the access token via /token with grant_type=refresh_token', async () => {
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
      codes: new Map(),
      refreshTokens: new Map([['rt-1', { sub: 'user-1' }]]),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const service = new OAuthService();
    const token = await service.refreshToken('rt-1');
    expect(token?.access_token).toBeTruthy();
    expect(token?.refresh_token).toBe('rotated-refresh');

    const req = state.requests.find((r) => r.url === `${AUTH_BASE}/token`);
    const body = new URLSearchParams(req?.body ?? '');
    expect(body.get('grant_type')).toBe('refresh_token');
    expect(body.get('refresh_token')).toBe('rt-1');
    expect(body.get('client_id')).toBe('test-client');
    expect(body.get('client_secret')).toBe('test-secret');
  });

  // -----------------------------------------------------------------------

  it('verifies the access token via the discovered jwks_uri (useUser)', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        scopes: ['openid', 'profile', 'email'],
        pkce: 'never',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map([['code-4', { redirectUri: undefined, subject: { sub: 'user-4', email: 'op@ex.com' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
      clientId: 'test-client',
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const { useUser } = await import('../../src/oauth/oauth.utils');
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      hp.route('GET', '/me', async () => {
        const user = await useUser<{ uid: string; sub?: string; email?: string }>({ redirect: false });
        return user ?? null;
      });

      const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      const loginCookie = loginRes.headers.get('set-cookie') ?? '';
      const authorizeUrl4 = new URL(loginRes.headers.get('location') ?? '');
      const stateParam = authorizeUrl4.searchParams.get('state');
      const nonce4 = authorizeUrl4.searchParams.get('nonce') ?? undefined;
      const code4 = state.codes.get('code-4');
      if (code4) {
        code4.redirectUri = `http://localhost:${port}/auth/callback`;
        code4.idTokenClaims = { sub: 'user-4', nonce: nonce4 };
      }

      const cbRes = await fetch(`http://localhost:${port}/auth/callback?code=code-4&state=${stateParam}`, {
        headers: { Cookie: loginCookie },
        redirect: 'manual',
      });
      const sessionCookie = cbRes.headers.get('set-cookie') ?? '';

      const meRes = await fetch(`http://localhost:${port}/me`, { headers: { Cookie: sessionCookie } });
      const me = (await meRes.json()) as { sub?: string; email?: string };
      expect(me.sub).toBe('user-4');
      expect(me.email).toBe('op@ex.com');
      expect(state.requests.some((r) => r.url === `${AUTH_BASE}/.well-known/jwks.json`)).toBe(true);
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('falls back to userinfo when the access token cannot be verified as a JWT', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        scopes: ['read:user'],
        pkce: 'never',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map([['code-5', { redirectUri: undefined, subject: {}, opaque: true }]]),
      refreshTokens: new Map(),
      userInfo: { sub: 'user-5', email: 'opaque@ex.com' },
      jwks: { keys: [publicJwk] },
      clientId: 'test-client',
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const { useUser } = await import('../../src/oauth/oauth.utils');
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      hp.route('GET', '/me', async () => {
        const user = await useUser<{ uid: string; sub?: string; email?: string }>({ redirect: false });
        return user ?? null;
      });

      const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      const loginCookie = loginRes.headers.get('set-cookie') ?? '';
      const authorizeUrl5 = new URL(loginRes.headers.get('location') ?? '');
      const stateParam = authorizeUrl5.searchParams.get('state');
      const code5 = state.codes.get('code-5');
      if (code5) code5.redirectUri = `http://localhost:${port}/auth/callback`;

      const cbRes = await fetch(`http://localhost:${port}/auth/callback?code=code-5&state=${stateParam}`, {
        headers: { Cookie: loginCookie },
        redirect: 'manual',
      });
      const sessionCookie = cbRes.headers.get('set-cookie') ?? '';

      const meRes = await fetch(`http://localhost:${port}/me`, { headers: { Cookie: sessionCookie } });
      const me = (await meRes.json()) as { sub?: string; email?: string };
      expect(me.sub).toBe('user-5');
      expect(me.email).toBe('opaque@ex.com');
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  async function loginAndCallback(port: number, state: MockState, code: string): Promise<string> {
    const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
    const loginCookie = loginRes.headers.get('set-cookie') ?? '';
    const authorizeUrl = new URL(loginRes.headers.get('location') ?? '');
    const stateParam = authorizeUrl.searchParams.get('state');
    const entry = state.codes.get(code);
    if (entry) entry.redirectUri = `http://localhost:${port}/auth/callback`;
    const cbRes = await fetch(`http://localhost:${port}/auth/callback?code=${code}&state=${stateParam}`, {
      headers: { Cookie: loginCookie },
      redirect: 'manual',
    });
    return cbRes.headers.get('set-cookie') ?? '';
  }

  it('useUser() rejects an access token whose aud does not match oauth.audience', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        audience: 'api://expected',
        scopes: [],
        pkce: 'never',
      },
    });
    // Access token minted for a *sibling* service (audience-confusion attempt).
    const state: MockState = {
      requests: [],
      codes: new Map([['code-aud', { subject: { sub: 'aud-user', aud: 'api://sibling' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const { useUser } = await import('../../src/oauth/oauth.utils');
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port ?? 0;
      hp.route('GET', '/me', async () => {
        const user = await useUser<{ sub?: string }>({ redirect: false });
        return user ?? null;
      });
      const sessionCookie = await loginAndCallback(port, state, 'code-aud');
      const meRes = await fetch(`http://localhost:${port}/me`, { headers: { Cookie: sessionCookie } });
      const me = (await meRes.json()) as { sub?: string } | null;
      // The wrong-aud token must not authenticate as aud-user (no id_token, empty userinfo).
      expect(me?.sub).toBeUndefined();
    } finally {
      await app.stop();
    }
  });

  it('useUser() does not rescue a wrong-audience token via the userinfo fallback', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        audience: 'api://expected',
        scopes: [],
        pkce: 'never',
      },
    });
    // Access token minted for a *sibling* service; the IdP considers it active,
    // so its userinfo endpoint happily returns claims for it. With
    // oauth.audience configured, useUser() must NOT consult userinfo — the
    // failed audience-bound verify is final.
    const state: MockState = {
      requests: [],
      codes: new Map([['code-aud-ui', { subject: { sub: 'sibling-user', aud: 'api://sibling' } }]]),
      refreshTokens: new Map(),
      userInfo: { sub: 'sibling-user', email: 'sibling@ex.com' },
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const { useUser } = await import('../../src/oauth/oauth.utils');
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port ?? 0;
      hp.route('GET', '/me', async () => {
        const user = await useUser<{ sub?: string }>({ redirect: false });
        return user ?? null;
      });
      const sessionCookie = await loginAndCallback(port, state, 'code-aud-ui');
      const meRes = await fetch(`http://localhost:${port}/me`, { headers: { Cookie: sessionCookie } });
      const me = (await meRes.json()) as { sub?: string } | null;
      // Not authenticated — even though userinfo would have vouched for the token.
      expect(me?.sub).toBeUndefined();
      // And the userinfo endpoint was never even consulted.
      expect(state.requests.some((r) => r.url === `${AUTH_BASE}/userinfo`)).toBe(false);
    } finally {
      await app.stop();
    }
  });

  it('useUser() accepts an access token whose aud matches oauth.audience', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        audience: 'api://expected',
        scopes: [],
        pkce: 'never',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map([['code-aud-ok', { subject: { sub: 'aud-user', aud: 'api://expected' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const { useUser } = await import('../../src/oauth/oauth.utils');
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port ?? 0;
      hp.route('GET', '/me', async () => {
        const user = await useUser<{ sub?: string }>({ redirect: false });
        return user ?? null;
      });
      const sessionCookie = await loginAndCallback(port, state, 'code-aud-ok');
      const meRes = await fetch(`http://localhost:${port}/me`, { headers: { Cookie: sessionCookie } });
      const me = (await meRes.json()) as { sub?: string } | null;
      expect(me?.sub).toBe('aud-user');
    } finally {
      await app.stop();
    }
  });

  it('refreshToken() de-duplicates concurrent refreshes (single-flight)', async () => {
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
      codes: new Map(),
      refreshTokens: new Map([['rt-shared', { sub: 'concurrent-user' }]]),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const service = new OAuthService();
    const results = await Promise.all(Array.from({ length: 5 }, () => service.refreshToken('rt-shared')));

    // Every caller gets a token, but only one POST hits the token endpoint.
    expect(results.every((r) => r?.access_token)).toBe(true);
    const tokenPosts = state.requests.filter((r) => r.url === `${AUTH_BASE}/token` && r.method === 'POST').length;
    expect(tokenPosts).toBe(1);
  });

  // -----------------------------------------------------------------------

  it('matches callbacks against multiple pending login states in the same session', async () => {
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
      codes: new Map([
        ['code-first', { redirectUri: undefined, subject: { sub: 'user-first' } }],
        ['code-second', { redirectUri: undefined, subject: { sub: 'user-second' } }],
      ]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
      clientId: 'test-client',
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const callbackUri = `http://localhost:${port}/auth/callback`;
      state.codes.get('code-first')!.redirectUri = callbackUri;
      state.codes.get('code-second')!.redirectUri = callbackUri;

      const firstLogin = await fetch(`http://localhost:${port}/login?redirect=/first`, { redirect: 'manual' });
      const firstCookie = firstLogin.headers.get('set-cookie') ?? '';
      const firstAuthorize = new URL(firstLogin.headers.get('location') ?? '');
      const firstState = firstAuthorize.searchParams.get('state') ?? '';
      expect(firstState).toBeTruthy();

      const secondLogin = await fetch(`http://localhost:${port}/login?redirect=/second`, {
        headers: { Cookie: firstCookie },
        redirect: 'manual',
      });
      const secondCookie = secondLogin.headers.get('set-cookie') ?? '';
      const secondAuthorize = new URL(secondLogin.headers.get('location') ?? '');
      const secondState = secondAuthorize.searchParams.get('state') ?? '';
      expect(secondState).toBeTruthy();
      expect(secondState).not.toBe(firstState);

      const firstCallback = await fetch(`http://localhost:${port}/auth/callback?code=code-first&state=${firstState}`, {
        headers: { Cookie: secondCookie },
        redirect: 'manual',
      });
      expect(firstCallback.status).toBe(302);
      expect(firstCallback.headers.get('location')).toBe('/first');

      const cookieAfterFirstCallback = firstCallback.headers.get('set-cookie') ?? '';
      const firstReplay = await fetch(`http://localhost:${port}/auth/callback?code=code-first&state=${firstState}`, {
        headers: { Cookie: cookieAfterFirstCallback },
        redirect: 'manual',
      });
      expect(firstReplay.status).toBe(302);
      expect(firstReplay.headers.get('location')).toBe('/');

      const secondCallback = await fetch(
        `http://localhost:${port}/auth/callback?code=code-second&state=${secondState}`,
        {
          headers: { Cookie: cookieAfterFirstCallback },
          redirect: 'manual',
        },
      );
      expect(secondCallback.status).toBe(302);
      expect(secondCallback.headers.get('location')).toBe('/second');

      const exchangedCodes = state.requests
        .filter((request) => request.url === `${AUTH_BASE}/token` && request.method === 'POST')
        .map((request) => new URLSearchParams(request.body ?? '').get('code'));
      expect(exchangedCodes).toEqual(['code-first', 'code-second']);
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('rejects callbacks with a mismatched state', async () => {
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
      codes: new Map(),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const loginRes = await fetch(`http://localhost:${port}/login`, { redirect: 'manual' });
      const cookie = loginRes.headers.get('set-cookie') ?? '';
      const cbRes = await fetch(`http://localhost:${port}/auth/callback?code=x&state=wrong`, {
        headers: { Cookie: cookie },
        redirect: 'manual',
      });
      expect(cbRes.status).toBe(400);
      expect(cbRes.headers.get('content-type')).toContain('application/json');
      expect(await cbRes.json()).toEqual({ error: 'invalid_state', error_description: 'state mismatch' });
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('redirects a replayed callback to defaultLoginRedirectUrl when the session is signed in', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        defaultLoginRedirectUrl: '/home',
        scopes: [],
        pkce: 'never',
      },
    });
    const state: MockState = {
      requests: [],
      codes: new Map([['code-once', { redirectUri: undefined, subject: { sub: 'user-once' } }]]),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
      clientId: 'test-client',
    };
    globalThis.fetch = makeFetch(state);

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      state.codes.get('code-once')!.redirectUri = `http://localhost:${port}/auth/callback`;

      const login = await fetch(`http://localhost:${port}/login?redirect=/dashboard`, { redirect: 'manual' });
      const loginCookie = login.headers.get('set-cookie') ?? '';
      const stateParam = new URL(login.headers.get('location') ?? '').searchParams.get('state') ?? '';
      const callbackUrl = `http://localhost:${port}/auth/callback?code=code-once&state=${stateParam}`;
      const browserAccept = 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8';

      const first = await fetch(callbackUrl, {
        headers: { Cookie: loginCookie, Accept: browserAccept },
        redirect: 'manual',
      });
      expect(first.status).toBe(302);
      expect(first.headers.get('location')).toBe('/dashboard');
      const signedInCookie = first.headers.get('set-cookie') ?? '';

      const replay = await fetch(callbackUrl, {
        headers: { Cookie: signedInCookie, Accept: browserAccept },
        redirect: 'manual',
      });
      expect(replay.status).toBe(302);
      expect(replay.headers.get('location')).toBe('/home');

      const exchanges = state.requests.filter((r) => r.url === `${AUTH_BASE}/token` && r.method === 'POST');
      expect(exchanges).toHaveLength(1);
    } finally {
      await app.stop();
    }
  });

  // -----------------------------------------------------------------------

  it('answers an unknown state without a signed-in session with an HTML page for browsers', async () => {
    setConfig({
      oauth: {
        clientId: 'test-client',
        clientSecret: 'test-secret',
        discoveryUri: DISCOVERY_URI,
        callbackRoute: '/auth/callback',
        loginRoute: '/sign-in',
        scopes: [],
        pkce: 'never',
      },
    });
    globalThis.fetch = makeFetch({
      requests: [],
      codes: new Map(),
      refreshTokens: new Map(),
      userInfo: {},
      jwks: { keys: [publicJwk] },
    });

    const hp = http({ port: 0 });
    const app = application().use(hp).use(config()).use(oAuth2());
    await app.start();
    try {
      const port = hp.getServer()?.port;
      const res = await fetch(`http://localhost:${port}/auth/callback?code=x&state=consumed`, {
        headers: { Accept: 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8' },
        redirect: 'manual',
      });
      expect(res.status).toBe(400);
      expect(res.headers.get('content-type')).toContain('text/html');
      expect(res.headers.get('cache-control')).toBe('no-store');
      const html = await res.text();
      expect(html).toContain('This sign-in link has expired');
      expect(html).toContain('<a href="/sign-in">Sign in again</a>');
      expect(html).not.toContain('error_description');
      expect(html).not.toContain('state mismatch');
    } finally {
      await app.stop();
    }
  });
});
