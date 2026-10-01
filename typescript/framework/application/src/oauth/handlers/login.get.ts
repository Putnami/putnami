import { useConfig, useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import { HttpResponse } from '../../http/http-response';
import { setSession, useSession } from '../../session';
import { OAuthConfig } from '../oauth.config';
import { generateCodeChallenge, generateCodeVerifier, useOAuthService } from '../oauth.service';
import { appendOAuthPendingSession, OAUTH_PENDING_SESSION_KEY, type OAuthPendingSession } from '../oauth-session';
import { CallbackHandler } from './callback.get';

const logger = useLogger('oauth');

function isOidcLogin(scopes: string[]): boolean {
  return scopes.includes('openid');
}

export async function LoginHandler(ctx: HttpRequestContext): Promise<HttpResponse> {
  const config = useConfig(OAuthConfig);
  const oauthService = useOAuthService();
  const { code } = ctx.queryParams();

  // Back-compat: when loginRoute and callbackRoute are the same path,
  // a request with `?code` is actually the callback hitting /login.
  if (code && (config.callbackRoute ?? config.loginRoute) === config.loginRoute) {
    return CallbackHandler(ctx);
  }

  const usePkce = await oauthService.shouldUsePkce();
  const state = crypto.randomUUID();
  const codeVerifier = usePkce ? generateCodeVerifier() : undefined;
  const codeChallenge = codeVerifier ? await generateCodeChallenge(codeVerifier) : undefined;
  const nonce = isOidcLogin(config.scopes) ? crypto.randomUUID() : undefined;

  const redirectUri = config.redirectUri ?? oauthService.deriveRedirectUri();
  const { redirect: redirectTo } = ctx.queryParams();

  const pending: OAuthPendingSession = {
    state,
    codeVerifier,
    nonce,
    redirectTo: safeRedirectPath(redirectTo) ?? config.defaultLoginRedirectUrl,
    redirectUri,
  };
  setSession<OAuthPendingSession[]>(
    OAUTH_PENDING_SESSION_KEY,
    appendOAuthPendingSession(useSession<unknown>(OAUTH_PENDING_SESSION_KEY), pending),
  );

  const url = await oauthService.authorizeUrl({
    state,
    codeChallenge,
    codeChallengeMethod: codeChallenge ? 'S256' : undefined,
    nonce,
    redirectUri,
  });

  logger.debug('Login initiated, redirecting to authorization endpoint', { pkce: !!codeChallenge, nonce: !!nonce });
  return HttpResponse.redirect(url);
}

/**
 * Returns the value when it's a same-origin path (`/foo/...`), otherwise undefined.
 *
 * Blocks open-redirect attempts:
 * - `//evil.example.com` (protocol-relative).
 * - `/\evil.example.com` (backslash; some browsers/proxies normalize it to `/`).
 * - `/%2Fevil.example.com`, `/%5Cevil.example.com` (percent-encoded `/` or `\`).
 * - `http:/...`, `javascript:...`, and anything else that doesn't parse to a same-host path.
 *
 * Limited to 2048 characters as defense-in-depth.
 */
export function safeRedirectPath(value: unknown): string | undefined {
  if (typeof value !== 'string' || value.length === 0 || value.length > 2048) return undefined;
  if (!value.startsWith('/')) return undefined;
  const second = value.slice(1, 2);
  if (second === '/' || second === '\\') return undefined;
  if (/^\/(%2f|%2F|%5c|%5C)/.test(value)) return undefined;
  try {
    const probe = new URL(value, 'http://_safe_redirect_probe_');
    if (probe.host !== '_safe_redirect_probe_' || probe.protocol !== 'http:') return undefined;
    if (!probe.pathname.startsWith('/')) return undefined;
    return value;
  } catch {
    return undefined;
  }
}
