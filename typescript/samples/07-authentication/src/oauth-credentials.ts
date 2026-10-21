import { OAuthConfig, type Plugin } from '@putnami/application';
import { useConfig } from '@putnami/runtime';

/**
 * A `${NAME}` reference the configuration loader left in place: it replaces a
 * reference only when NAME is set or the reference names a default.
 */
const UNRESOLVED_REFERENCE = '${';

/** A credential is usable when it is not blank and holds no unresolved reference. */
function isUsableCredential(value: string | undefined): boolean {
  return typeof value === 'string' && value.trim().length > 0 && !value.includes(UNRESOLVED_REFERENCE);
}

/**
 * Refuses to start the application without OAuth client credentials.
 *
 * `oauth.clientId` and `oauth.clientSecret` are runtime configuration:
 * `conf/.env.yaml` reads them from `OAUTH_CLIENT_ID` and `OAUTH_CLIENT_SECRET`,
 * and a higher-priority config source can supply them instead. A missing or
 * blank value, and a value that is still a `${...}` reference to an unset
 * variable, count as missing. The check runs in `warmup`, when the application
 * starts, so `app()` and the build need no credential. Startup fails before any
 * plugin starts a server. The error names the missing settings, never a value.
 */
export const requireOAuthCredentials = (): Plugin => ({
  name: 'oauth-credentials',
  warmup() {
    const { clientId, clientSecret } = useConfig(OAuthConfig);
    const missing = [
      ...(isUsableCredential(clientId) ? [] : ['OAUTH_CLIENT_ID (oauth.clientId)']),
      ...(isUsableCredential(clientSecret) ? [] : ['OAUTH_CLIENT_SECRET (oauth.clientSecret)']),
    ];
    if (missing.length > 0) {
      throw new Error(`Missing required OAuth credentials: ${missing.join(', ')}`);
    }
  },
});
