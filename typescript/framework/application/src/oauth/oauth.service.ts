import { type InferConfig, useConfig, useContext, useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../http';
import { timingSafeEqual } from '../security/security.utils';
import { decodeJwtHeader, importPublicKey, type PublicKeyMaterial, verifyJwt, type VerifyOptions } from './oauth-jwt';
import { OAuthConfig } from './oauth.config';
import type { TokenInfo } from './token-info.types';

// Re-export so the public OAuth API (oauth barrel → @putnami/application) is unchanged
// after the JWT/crypto helpers moved into oauth-jwt.ts.
export type { VerifyOptions } from './oauth-jwt';

/**
 * Options for building an /authorize URL.
 */
export interface AuthorizeUrlOptions {
  /** Opaque CSRF token; the callback handler must compare it to the value stashed in the session. */
  state?: string;
  /** PKCE code challenge (base64url(SHA-256(verifier))). */
  codeChallenge?: string;
  /** PKCE method. Defaults to `S256` when `codeChallenge` is provided. */
  codeChallengeMethod?: 'S256' | 'plain';
  /** OIDC nonce, used to bind an id_token to a session. */
  nonce?: string;
  /** Override `redirect_uri`. When unset, falls back to config or `${domain}${callbackRoute ?? loginRoute}`. */
  redirectUri?: string;
  /** Override scopes. When unset, uses `oauth.scopes` from config. */
  scopes?: string[];
  /** Extra params forwarded verbatim to /authorize. */
  extra?: Record<string, string>;
}

/**
 * Options for exchanging an authorization code at /token.
 */
export interface ExchangeCodeOptions {
  code: string;
  /** Must match the `redirect_uri` sent at /authorize. */
  redirectUri?: string;
  /** PKCE verifier paired with the `code_challenge` sent at /authorize. */
  codeVerifier?: string;
}

/**
 * Resolved provider endpoints (config + discovery, with config winning on overlap).
 *
 * Endpoints are `undefined` when neither explicit config nor OIDC discovery
 * supplies them. There are no hardcoded provider fallbacks: callers must guard
 * with {@link requireEndpoint} (or an equivalent check) so a misconfigured
 * deployment fails loudly instead of silently talking to a default IdP.
 */
export interface ResolvedEndpoints {
  authorize?: string;
  token?: string;
  jwks?: string;
  userInfo?: string;
  signout?: string;
  issuer?: string;
  codeChallengeMethodsSupported?: string[];
}

/**
 * Returns `value` when set, otherwise throws a clear configuration error.
 *
 * Used to guard provider endpoints that have no hardcoded fallback: a missing
 * endpoint means the deployment is under-configured, which must surface as an
 * explicit error rather than a silent default.
 */
function requireEndpoint(value: string | undefined, label: string, configKey: string): string {
  if (!value) {
    throw new Error(
      `OAuth ${label} endpoint is not configured. Set oauth.${configKey} or oauth.discoveryUri ` +
        `so it can be resolved from the provider's OIDC discovery document.`,
    );
  }
  return value;
}

interface DiscoveryDocument {
  issuer?: string;
  authorization_endpoint?: string;
  token_endpoint?: string;
  userinfo_endpoint?: string;
  jwks_uri?: string;
  end_session_endpoint?: string;
  code_challenge_methods_supported?: string[];
  token_endpoint_auth_methods_supported?: string[];
}

const TEXT_ENCODER = new TextEncoder();

// ---------------------------------------------------------------------------
// Base64url + crypto helpers
// ---------------------------------------------------------------------------

function encodeBase64Url(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * `application/x-www-form-urlencoded` per RFC 3986 / HTML5. Differs from
 * `encodeURIComponent` on `!`, `'`, `(`, `)`, `*` (all encoded here) and on
 * space (`+` here vs `%20`). Required by RFC 6749 §2.3.1 for `client_secret_basic`.
 */
function formUrlEncode(value: string): string {
  return encodeURIComponent(value)
    .replace(/!/g, '%21')
    .replace(/'/g, '%27')
    .replace(/\(/g, '%28')
    .replace(/\)/g, '%29')
    .replace(/\*/g, '%2A')
    .replace(/%20/g, '+');
}

/**
 * Computes a session `expireAt` from a token response's `expires_in` (seconds).
 * Tolerates missing / non-positive values by falling back to the default; subtracts
 * a small clock-skew margin from the advertised lifetime.
 */
const DEFAULT_TOKEN_LIFETIME_SECONDS = 300;
export function computeExpireAt(expiresIn: number | undefined): number {
  const seconds = typeof expiresIn === 'number' && expiresIn > 0 ? expiresIn : DEFAULT_TOKEN_LIFETIME_SECONDS;
  return Date.now() + Math.max(0, seconds - 1) * 1000;
}

/**
 * Generates a PKCE verifier (high-entropy URL-safe string, RFC 7636 §4.1).
 */
export function generateCodeVerifier(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return encodeBase64Url(bytes);
}

/**
 * Derives the PKCE code_challenge for an S256 verifier.
 */
export async function generateCodeChallenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', TEXT_ENCODER.encode(verifier));
  return encodeBase64Url(new Uint8Array(digest));
}

// ---------------------------------------------------------------------------
// Module-level delegation
// ---------------------------------------------------------------------------

let _activeOAuthService: OAuthService | undefined;

/** @internal Called by OAuthPlugin during warmup/stop. */
export function setActiveOAuthService(service: OAuthService | undefined): void {
  _activeOAuthService = service;
}

/**
 * Returns the active OAuthService instance.
 *
 * @throws {Error} if no OAuthPlugin has been registered
 */
export function useOAuthService(): OAuthService {
  if (!_activeOAuthService) {
    throw new Error('OAuthService is not active. Add oAuth2() to your Application plugins.');
  }
  return _activeOAuthService;
}

// ---------------------------------------------------------------------------
// OAuthService
// ---------------------------------------------------------------------------

/**
 * OAuth2 / OpenID Connect client service.
 *
 * Implements the authorization-code flow, refresh tokens, client credentials,
 * JWT verification (RS256/ES256) and OIDC discovery. Most apps don't talk to this
 * directly — `useUser()` and `accessToken()` cover the usual cases.
 */
export class OAuthService {
  /** @internal Set by OAuthPlugin to forward programmatic overrides. */
  confInit?: Partial<InferConfig<typeof OAuthConfig>>;

  private static readonly CACHE_TTL = 60 * 60 * 1000; // 1 hour

  private clientTokenCache: { token: string; expireAt: number } | undefined;
  private _publicKeys: { keys: PublicKeyMaterial[]; cachedAt: number; uri: string } | undefined;
  private _discovery: { doc: DiscoveryDocument; cachedAt: number; uri: string } | undefined;
  private _discoveryInflight: Promise<DiscoveryDocument | undefined> | undefined;
  private readonly _refreshInflight = new Map<string, Promise<TokenInfo | undefined>>();

  private get config() {
    return useConfig(OAuthConfig, { confInit: this.confInit });
  }

  /**
   * Resolves provider endpoints from discovery (if configured) merged with
   * explicit config overrides. Cached for 1 hour.
   */
  async resolveEndpoints(): Promise<ResolvedEndpoints> {
    const cfg = this.config;
    const doc = await this.discover();

    return {
      authorize: cfg.authorizeUri || doc?.authorization_endpoint,
      token: cfg.tokenUri || doc?.token_endpoint,
      jwks: cfg.keysUri || doc?.jwks_uri,
      userInfo: cfg.userInfoUri || doc?.userinfo_endpoint,
      signout: cfg.signoutUri || doc?.end_session_endpoint,
      issuer: cfg.issuer || doc?.issuer,
      codeChallengeMethodsSupported: doc?.code_challenge_methods_supported,
    };
  }

  // -------------------------------------------------------------------------
  // Authorization
  // -------------------------------------------------------------------------

  /**
   * Builds the /authorize URL the browser must follow to start a login flow.
   *
   * The handler is expected to have already generated `state` (CSRF) and,
   * when applicable, a PKCE verifier/challenge pair and an OIDC nonce.
   */
  async authorizeUrl(options: AuthorizeUrlOptions = {}): Promise<string> {
    const endpoints = await this.resolveEndpoints();
    const cfg = this.config;
    const redirectUri = options.redirectUri ?? cfg.redirectUri ?? this.deriveRedirectUri();
    const scopes = options.scopes ?? cfg.scopes;

    const params = new URLSearchParams();
    params.set('response_type', 'code');
    if (cfg.clientId) params.set('client_id', cfg.clientId);
    if (redirectUri) params.set('redirect_uri', redirectUri);
    if (scopes && scopes.length > 0) params.set('scope', scopes.join(' '));
    if (options.state) params.set('state', options.state);
    if (options.nonce) params.set('nonce', options.nonce);
    if (options.codeChallenge) {
      params.set('code_challenge', options.codeChallenge);
      params.set('code_challenge_method', options.codeChallengeMethod ?? 'S256');
    }
    if (options.extra) {
      for (const [k, v] of Object.entries(options.extra)) params.set(k, v);
    }

    const authorize = requireEndpoint(endpoints.authorize, 'authorization', 'authorizeUri');
    const separator = authorize.includes('?') ? '&' : '?';
    return `${authorize}${separator}${params.toString()}`;
  }

  /**
   * Computes the redirect_uri from the current request when none was configured.
   * Falls back to `loginRoute` for back-compat with the single-route flow.
   */
  deriveRedirectUri(): string | undefined {
    try {
      const ctx = useContext<HttpRequestContext>();
      const cfg = this.config;
      const route = cfg.callbackRoute ?? cfg.loginRoute;
      return `${ctx.domain()}${route}`;
    } catch {
      return undefined;
    }
  }

  /**
   * Decides whether to use PKCE based on the configured policy and discovery.
   *
   * - `auto` (default): on when discovery advertises a challenge method; on when no
   *   discovery is configured (defense in depth — OAuth 2.1 / RFC 9700 recommend PKCE
   *   for confidential clients too, and many providers accept PKCE without advertising it).
   * - `always`: always on.
   * - `never`: always off.
   */
  async shouldUsePkce(): Promise<boolean> {
    const cfg = this.config;
    if (cfg.pkce === 'never') return false;
    if (cfg.pkce === 'always') return true;
    if (cfg.tokenEndpointAuthMethod === 'none') return true;
    if (!cfg.discoveryUri) return true;
    const endpoints = await this.resolveEndpoints();
    return (endpoints.codeChallengeMethodsSupported?.length ?? 0) > 0;
  }

  // -------------------------------------------------------------------------
  // Token exchange
  // -------------------------------------------------------------------------

  /**
   * Exchanges an authorization code for tokens.
   */
  async exchangeCode(options: ExchangeCodeOptions): Promise<TokenInfo | undefined> {
    const body: Record<string, string> = {
      grant_type: 'authorization_code',
      code: options.code,
    };
    const redirectUri = options.redirectUri ?? this.config.redirectUri ?? this.deriveRedirectUri();
    if (redirectUri) body['redirect_uri'] = redirectUri;
    if (options.codeVerifier) body['code_verifier'] = options.codeVerifier;
    return this.fetchToken(body);
  }

  /**
   * Refreshes an access token using a refresh token.
   *
   * Concurrent calls for the same refresh token are de-duplicated (single-flight):
   * a page firing several XHRs against an expired session would otherwise POST the
   * same refresh token N times, and with refresh-token rotation (OAuth 2.1) all but
   * the first would get `invalid_grant` → spurious logout. Callers share one request.
   */
  async refreshToken(refreshToken: string): Promise<TokenInfo | undefined> {
    const inflight = this._refreshInflight.get(refreshToken);
    if (inflight) return inflight;

    const promise = (async () => {
      try {
        return await this.fetchToken({ grant_type: 'refresh_token', refresh_token: refreshToken });
      } finally {
        this._refreshInflight.delete(refreshToken);
      }
    })();
    this._refreshInflight.set(refreshToken, promise);
    return promise;
  }

  /**
   * Retrieves a client-credentials access token. Cached until expiry.
   */
  async clientToken(): Promise<string | undefined> {
    if (!this.clientTokenCache || this.clientTokenCache.expireAt < Date.now()) {
      const tokenInfo = await this.fetchToken({ grant_type: 'client_credentials' });
      if (!tokenInfo) return undefined;
      this.clientTokenCache = {
        token: tokenInfo.access_token,
        expireAt: computeExpireAt(tokenInfo.expires_in),
      };
    }
    return this.clientTokenCache.token;
  }

  // -------------------------------------------------------------------------
  // JWT verification
  // -------------------------------------------------------------------------

  /**
   * Verifies and decodes a JWT (RS256 or ES256; the algorithm is taken from the
   * token header).
   *
   * Key selection:
   * - When the JWT header has a `kid`, only the JWK with that `kid` is tried.
   *   If the cached JWKS doesn't have it, the JWKS is refetched once before
   *   giving up (key rotation case).
   * - When the JWT header has no `kid`, every signing key is tried.
   */
  /**
   * Verification options derived from config that bind a token to *this* API:
   * the configured `oauth.audience` and `oauth.issuer`. Used to default
   * `useUser()` so a token minted for a sibling service on a shared IdP is not
   * accepted here (audience-confusion / token-substitution).
   *
   * Returns an empty object when neither is configured (verification then only
   * checks signature + `exp`/`nbf`); operators are warned about this elsewhere
   * (see {@link IdentityResolverMiddleware}).
   */
  configuredVerifyOptions(): VerifyOptions {
    const cfg = this.config;
    const options: VerifyOptions = {};
    if (cfg.issuer) options.issuer = cfg.issuer;
    if (cfg.audience) options.audience = cfg.audience;
    return options;
  }

  async verify<T>(token: string, options: VerifyOptions = {}): Promise<T | undefined> {
    const header = decodeJwtHeader(token);
    let candidates = await this.candidateKeysFor(header?.kid);
    if (candidates.length === 0 && header?.kid) {
      candidates = await this.candidateKeysFor(header.kid, /* forceRefresh */ true);
    }
    if (candidates.length === 0) {
      useLogger().warn('No matching JWK for token', { kid: header?.kid });
      return undefined;
    }

    const attempts = candidates.map(async (material) => {
      const key = await importPublicKey(material);
      return verifyJwt<T>(token, key, options);
    });

    try {
      return await Promise.any(attempts);
    } catch (err) {
      const errors =
        err instanceof AggregateError ? err.errors.map((e) => (e instanceof Error ? e.message : String(e))) : [];
      useLogger().warn('JWT verification failed', {
        keyCount: candidates.length,
        kid: header?.kid,
        errors,
      });
      return undefined;
    }
  }

  private async candidateKeysFor(kid: string | undefined, forceRefresh = false): Promise<PublicKeyMaterial[]> {
    const keys = await this.publicKeys(forceRefresh);
    if (keys.length === 0) return [];
    if (!kid) return keys;
    return keys.filter((k) => k.kind === 'jwk' && k.jwk.kid === kid);
  }

  /**
   * Verifies an OIDC `id_token`.
   *
   * Checks signature against the discovered JWKS, then validates `iss` against
   * the discovered/configured issuer, `aud` against the configured `clientId`,
   * `exp` / `nbf`, and — when `expectedNonce` is provided — the `nonce` claim.
   *
   * Returns the decoded payload on success, `undefined` otherwise.
   */
  async verifyIdToken<T = Record<string, unknown>>(
    idToken: string,
    options: { expectedNonce?: string; clockTolerance?: number } = {},
  ): Promise<T | undefined> {
    const cfg = this.config;
    const endpoints = await this.resolveEndpoints();
    const issuer = endpoints.issuer ?? cfg.issuer;
    const verifyOptions: VerifyOptions = {
      clockTolerance: options.clockTolerance,
    };
    if (issuer) verifyOptions.issuer = issuer;
    if (cfg.clientId) verifyOptions.audience = cfg.clientId;

    const payload = await this.verify<Record<string, unknown>>(idToken, verifyOptions);
    if (!payload) return undefined;

    if (options.expectedNonce !== undefined) {
      const tokenNonce = typeof payload['nonce'] === 'string' ? (payload['nonce'] as string) : undefined;
      if (!tokenNonce || !timingSafeEqual(tokenNonce, options.expectedNonce)) {
        useLogger().warn('id_token nonce mismatch');
        return undefined;
      }
    }

    return payload as T;
  }

  // -------------------------------------------------------------------------
  // Userinfo
  // -------------------------------------------------------------------------

  /**
   * Calls the userinfo endpoint with the given access token and returns the
   * decoded claims. Returns undefined when no userinfo endpoint is configured
   * (or discovered) or when the call fails.
   */
  async userInfo<T = Record<string, unknown>>(accessToken: string): Promise<T | undefined> {
    const endpoints = await this.resolveEndpoints();
    if (!endpoints.userInfo) return undefined;

    try {
      const res = await fetch(endpoints.userInfo, {
        headers: { Accept: 'application/json', Authorization: `Bearer ${accessToken}` },
        signal: AbortSignal.timeout(5000),
      });
      if (!res.ok) {
        useLogger().warn('Userinfo request failed', { status: res.status, uri: endpoints.userInfo });
        return undefined;
      }
      return (await res.json()) as T;
    } catch (err) {
      useLogger().warn('Userinfo request errored', { error: err instanceof Error ? err.message : String(err) });
      return undefined;
    }
  }

  // -------------------------------------------------------------------------
  // Discovery
  // -------------------------------------------------------------------------

  private async discover(): Promise<DiscoveryDocument | undefined> {
    const uri = this.config.discoveryUri;
    if (!uri) return undefined;

    const now = Date.now();
    if (this._discovery && this._discovery.uri === uri && now - this._discovery.cachedAt < OAuthService.CACHE_TTL) {
      return this._discovery.doc;
    }

    if (this._discoveryInflight) return this._discoveryInflight;

    this._discoveryInflight = (async () => {
      try {
        const res = await fetch(uri, {
          headers: { Accept: 'application/json' },
          signal: AbortSignal.timeout(10_000),
        });
        if (!res.ok) {
          useLogger().warn('OIDC discovery failed', { status: res.status, uri });
          return undefined;
        }
        const doc = (await res.json()) as DiscoveryDocument;
        this._discovery = { doc, cachedAt: Date.now(), uri };
        return doc;
      } catch (err) {
        useLogger().warn('OIDC discovery errored', { error: err instanceof Error ? err.message : String(err), uri });
        return undefined;
      } finally {
        this._discoveryInflight = undefined;
      }
    })();

    return this._discoveryInflight;
  }

  // -------------------------------------------------------------------------
  // JWKS
  // -------------------------------------------------------------------------

  /**
   * Fetches and caches signing keys for JWT verification. Honors discovery's
   * `jwks_uri` when configured, otherwise uses `oauth.keysUri`.
   *
   * Accepts standard JWKS responses (`{ keys: [...] }`).
   */
  private async publicKeys(forceRefresh = false): Promise<PublicKeyMaterial[]> {
    const endpoints = await this.resolveEndpoints();
    const uri = requireEndpoint(endpoints.jwks, 'JWKS', 'keysUri');
    const now = Date.now();
    if (
      !forceRefresh &&
      this._publicKeys &&
      this._publicKeys.uri === uri &&
      now - this._publicKeys.cachedAt < OAuthService.CACHE_TTL
    ) {
      return this._publicKeys.keys;
    }

    try {
      const res = await fetch(uri, {
        headers: { Accept: 'application/json' },
        signal: AbortSignal.timeout(10_000),
      });
      if (res.status !== 200) {
        useLogger().warn('Failed to fetch public keys', { status: res.status, keysUri: uri });
        return [];
      }
      const data = await res.json();
      const keys = this.parseKeys(data, uri);
      this._publicKeys = { keys, cachedAt: now, uri };
      return keys;
    } catch (err) {
      useLogger().warn('JWKS request errored', {
        error: err instanceof Error ? err.message : String(err),
        keysUri: uri,
      });
      return [];
    }
  }

  private parseKeys(data: unknown, uri: string): PublicKeyMaterial[] {
    if (this.isJwks(data)) {
      const sigKeys = data.keys.filter((key) => key.use === 'sig' || !key.use);
      return sigKeys.map((jwk) => ({ kind: 'jwk' as const, jwk: jwk as JsonWebKey & { kid?: string } }));
    }
    useLogger().warn('Unknown keys format, expected JWKS', { keysUri: uri });
    return [];
  }

  private isJwks(data: unknown): data is { keys: Array<JsonWebKey & { use?: string; kid?: string }> } {
    return (
      typeof data === 'object' && data !== null && 'keys' in data && Array.isArray((data as { keys: unknown }).keys)
    );
  }

  // -------------------------------------------------------------------------
  // Token endpoint request
  // -------------------------------------------------------------------------

  private async fetchToken(body: Record<string, string>): Promise<TokenInfo | undefined> {
    const cfg = this.config;
    const endpoints = await this.resolveEndpoints();
    const tokenUri = requireEndpoint(endpoints.token, 'token', 'tokenUri');
    const authMethod = cfg.tokenEndpointAuthMethod;

    const form = new URLSearchParams();
    for (const [k, v] of Object.entries(body)) form.set(k, v);

    const headers: Record<string, string> = {
      'Content-Type': 'application/x-www-form-urlencoded',
      Accept: 'application/json',
    };

    if (authMethod === 'client_secret_basic') {
      if (!cfg.clientId) {
        useLogger().error('client_secret_basic requires clientId');
        return undefined;
      }
      // RFC 6749 §2.3.1: credentials are form-urlencoded before base64.
      const credentials = btoa(`${formUrlEncode(cfg.clientId)}:${formUrlEncode(cfg.clientSecret ?? '')}`);
      headers['Authorization'] = `Basic ${credentials}`;
      form.set('client_id', cfg.clientId);
    } else if (authMethod === 'none') {
      if (cfg.clientId) form.set('client_id', cfg.clientId);
    } else {
      if (cfg.clientId) form.set('client_id', cfg.clientId);
      if (cfg.clientSecret) form.set('client_secret', cfg.clientSecret);
    }

    let res: Response;
    try {
      res = await fetch(tokenUri, {
        method: 'POST',
        headers,
        body: form.toString(),
        signal: AbortSignal.timeout(cfg.tokenRequestTimeoutMs),
      });
    } catch (err) {
      useLogger().error('OAuth token request errored', {
        error: err instanceof Error ? err.message : String(err),
        grantType: body['grant_type'],
        tokenUri,
      });
      return undefined;
    }

    if (res.status === 200) {
      return (await res.json()) as TokenInfo;
    }

    const errorBody = await res.text().catch(() => 'Unable to read response body');
    useLogger().error('OAuth token request failed', {
      status: res.status,
      grantType: body['grant_type'],
      tokenUri,
      error: errorBody,
    });
    return undefined;
  }
}
