import { useConfig, useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import { HttpResponse } from '../../http/http-response';
import { timingSafeEqual } from '../../security/security.utils';
import { deleteSession, setSession, useSession } from '../../session';
import { OAuthConfig } from '../oauth.config';
import { computeExpireAt, useOAuthService } from '../oauth.service';
import {
  normalizeOAuthPendingSessions,
  OAUTH_PENDING_SESSION_KEY,
  OAUTH_SESSION_KEY,
  type OAuthPendingSession,
  type OAuthSession,
} from '../oauth-session';
import { type CallbackFailure, callbackFailureResponse } from './callback-failure';

const logger = useLogger('oauth');

/**
 * True when the session already holds an access token that is still valid or
 * can be refreshed.
 */
function hasUsableOAuthSession(): boolean {
  const session = useSession<OAuthSession>(OAUTH_SESSION_KEY);
  if (!session?.accessToken) return false;
  return session.expireAt > Date.now() || !!session.refreshToken;
}

function storePendingSessions(sessions: OAuthPendingSession[]): void {
  if (sessions.length > 0) {
    setSession<OAuthPendingSession[]>(OAUTH_PENDING_SESSION_KEY, sessions);
  } else {
    deleteSession(OAUTH_PENDING_SESSION_KEY);
  }
}

export async function CallbackHandler(ctx: HttpRequestContext): Promise<HttpResponse> {
  const config = useConfig(OAuthConfig);
  const oauthService = useOAuthService();
  const query = ctx.queryParams();
  const code = typeof query['code'] === 'string' ? query['code'] : undefined;
  const state = typeof query['state'] === 'string' ? query['state'] : undefined;
  const error = typeof query['error'] === 'string' ? query['error'] : undefined;
  const errorDescription = typeof query['error_description'] === 'string' ? query['error_description'] : undefined;

  const pendingSessions = normalizeOAuthPendingSessions(useSession<unknown>(OAUTH_PENDING_SESSION_KEY));
  const pendingIndex =
    state === undefined ? -1 : pendingSessions.findIndex((pending) => timingSafeEqual(pending.state, state));
  const pending = pendingIndex >= 0 ? pendingSessions[pendingIndex] : undefined;
  const fail = (failure: CallbackFailure) => callbackFailureResponse(ctx, config.loginRoute, failure);

  // A callback whose state matches no pending request exchanges no code. A
  // session that is already signed in goes to defaultLoginRedirectUrl; any
  // other session gets a failure that offers a new sign-in.
  const rejectUnknownState = (): HttpResponse => {
    const details = { hasPending: pendingSessions.length > 0, hasState: !!state };
    if (hasUsableOAuthSession()) {
      logger.info('OAuth callback state is unknown or already used; the session is already signed in', details);
      return HttpResponse.redirect(config.defaultLoginRedirectUrl);
    }
    logger.warn('OAuth callback state mismatch', details);
    return fail({ error: 'invalid_state', description: 'state mismatch', status: 400 });
  };

  if (!pending && (state !== undefined || error !== undefined)) {
    return rejectUnknownState();
  }

  if (pending) {
    storePendingSessions(pendingSessions.filter((_, index) => index !== pendingIndex));
  }

  if (error) {
    logger.warn('OAuth callback returned error', { error, errorDescription });
    return fail({ error, description: errorDescription ?? 'OAuth authorization failed', status: 401 });
  }

  if (!code) {
    logger.warn('OAuth callback missing code');
    return fail({ error: 'invalid_request', description: 'missing code', status: 400 });
  }

  if (!pending) {
    return rejectUnknownState();
  }

  const token = await oauthService.exchangeCode({
    code,
    redirectUri: pending.redirectUri,
    codeVerifier: pending.codeVerifier,
  });

  if (!token?.access_token) {
    logger.warn('OAuth token exchange failed');
    return fail({ error: 'token_exchange_failed', description: 'no access token returned', status: 401 });
  }

  // OIDC: when an id_token is returned, validate it before trusting it.
  // Signature, iss, aud=clientId, exp/nbf, and (when sent) nonce binding.
  if (token.id_token) {
    const idClaims = await oauthService.verifyIdToken(token.id_token, { expectedNonce: pending.nonce });
    if (!idClaims) {
      logger.warn('OAuth id_token validation failed');
      return fail({ error: 'invalid_id_token', description: 'id_token validation failed', status: 401 });
    }
  } else if (pending.nonce) {
    // We asked for OIDC (nonce was sent), but no id_token came back.
    logger.warn('OAuth: nonce was sent but no id_token returned');
    return fail({ error: 'missing_id_token', description: 'id_token expected for OIDC flow', status: 401 });
  }

  const session: OAuthSession = {
    accessToken: token.access_token,
    refreshToken: token.refresh_token,
    idToken: token.id_token,
    expireAt: computeExpireAt(token.expires_in),
  };
  setSession<OAuthSession>(OAUTH_SESSION_KEY, session);

  const redirectTo = pending.redirectTo || config.defaultLoginRedirectUrl;
  logger.with('oauth', { action: 'login', outcome: 'success', redirectTo });
  return HttpResponse.redirect(redirectTo);
}
