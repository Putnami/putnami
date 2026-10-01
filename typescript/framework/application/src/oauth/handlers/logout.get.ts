import { useConfig, useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import { redirect } from '../../http/redirect.utils';
import { deleteSessionAll, useSession } from '../../session';
import { OAuthConfig } from '../oauth.config';
import { useOAuthService } from '../oauth.service';
import { OAUTH_SESSION_KEY, type OAuthSession } from '../oauth-session';

const logger = useLogger('oauth');

/**
 * OIDC RP-Initiated Logout 1.0.
 *
 * Reads `id_token` from the session before clearing it, then redirects to the
 * provider's `end_session_endpoint` with `id_token_hint` and (when configured)
 * `post_logout_redirect_uri`.
 */
export async function LogoutHandler(_ctx: HttpRequestContext) {
  const config = useConfig(OAuthConfig);
  const oauthService = useOAuthService();
  const session = useSession<OAuthSession>(OAUTH_SESSION_KEY);
  const idTokenHint = session?.idToken;

  deleteSessionAll();

  const endpoints = await oauthService.resolveEndpoints();
  if (!endpoints.signout) {
    throw new Error(
      'OAuth end-session endpoint is not configured. Set oauth.signoutUri or oauth.discoveryUri ' +
        "so it can be resolved from the provider's OIDC discovery document.",
    );
  }
  const url = new URL(endpoints.signout);
  if (idTokenHint) url.searchParams.set('id_token_hint', idTokenHint);
  if (config.postLogoutRedirectUrl) url.searchParams.set('post_logout_redirect_uri', config.postLogoutRedirectUrl);

  logger.with('oauth', { action: 'logout', outcome: 'success' });
  return redirect(url.toString());
}
