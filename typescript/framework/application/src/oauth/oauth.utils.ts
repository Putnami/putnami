import { useConfig, useContext } from '@putnami/runtime';
import { type HttpRequestContext, redirect } from '../http';
import { setSession, useSession } from '../session';
import { OAuthConfig } from './oauth.config';
import { computeExpireAt, useOAuthService } from './oauth.service';
import { OAUTH_SESSION_KEY, type OAuthSession } from './oauth-session';
import type { User } from './user.types';

export interface AccessTokenOptions {
  /**
   * Whether to redirect to the OAuth login route when no token is available.
   * Defaults to true to preserve web login behavior.
   */
  redirect?: boolean;
}

/**
 * Retrieves a client-credentials access token from the OAuth provider.
 */
export const clientToken = async (): Promise<string | undefined> => useOAuthService().clientToken();

/**
 * Retrieves the current user's access token.
 *
 * Resolution order:
 * 1. `Authorization: Bearer` header
 * 2. Session-stored access token (with automatic refresh)
 * 3. Redirect to the OAuth login route (when `redirect: true`, default)
 */
export const accessToken = async (options: AccessTokenOptions = {}): Promise<string | undefined> => {
  const { req } = useContext<HttpRequestContext>();
  const authorization = req.headers.get('Authorization') ?? req.headers.get('authorization');
  if (authorization?.startsWith('Bearer ')) {
    const token = authorization.slice('Bearer '.length).trim();
    if (token) return token;
  }

  const oauthService = useOAuthService();
  let session = useSession<OAuthSession>(OAUTH_SESSION_KEY);
  const shouldRedirect = options.redirect ?? true;

  if (!session?.accessToken) {
    if (shouldRedirect) {
      throw redirect(loginUrl());
    }
    return undefined;
  }

  if (session.expireAt > Date.now()) return session.accessToken;

  if (session.refreshToken) {
    const token = await oauthService.refreshToken(session.refreshToken);
    if (token) {
      session = {
        accessToken: token.access_token,
        refreshToken: token.refresh_token ?? session.refreshToken,
        idToken: token.id_token ?? session.idToken,
        expireAt: computeExpireAt(token.expires_in),
      };
      setSession<OAuthSession>(OAUTH_SESSION_KEY, session);
      return session.accessToken;
    }
  }

  if (shouldRedirect) {
    throw redirect(loginUrl());
  }
  return undefined;
};

/**
 * Retrieves and verifies the current user's identity.
 *
 * Sources (in order):
 * 1. The access token, when it's a JWT signed by the discovered JWKS
 * 2. The id_token from the OIDC session
 * 3. The userinfo endpoint (when configured/discovered) — only when neither
 *    `oauth.audience` nor `oauth.issuer` is configured (see below)
 */
export const useUser = async <T extends User>(options: AccessTokenOptions = {}): Promise<T | undefined> => {
  const oauthService = useOAuthService();
  const token = await accessToken(options);
  if (!token) return undefined;

  // Bind the access token to this API's configured audience/issuer so a token
  // minted for a sibling service on a shared IdP cannot authenticate here
  // (audience-confusion / token-substitution). No-op when neither is configured.
  const boundOptions = oauthService.configuredVerifyOptions();
  const fromAccess = await oauthService.verify<T>(token, boundOptions);
  if (fromAccess) return fromAccess;

  const session = useSession<OAuthSession>(OAUTH_SESSION_KEY);
  if (session?.idToken) {
    const fromId = await oauthService.verify<T>(session.idToken);
    if (fromId) return fromId;
  }

  // The userinfo endpoint only proves the token is active at the IdP — it does
  // NOT validate `aud`/`iss`. When audience/issuer binding is configured, a
  // userinfo fallback would rescue a token minted for a sibling service and
  // undo the binding above, so the failed verify is final (this also matches
  // IdentityResolverMiddleware, which never consults userinfo). The fallback
  // stays available only for setups without audience/issuer binding, e.g.
  // providers issuing opaque access tokens.
  if (boundOptions.audience !== undefined || boundOptions.issuer !== undefined) {
    return undefined;
  }

  return oauthService.userInfo<T>(token);
};

function loginUrl(): string {
  return useConfig(OAuthConfig).loginRoute;
}
