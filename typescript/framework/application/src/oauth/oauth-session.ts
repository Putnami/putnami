/**
 * Session storage key for OAuth session data (access/refresh tokens).
 */
export const OAUTH_SESSION_KEY = 'oauth';

/**
 * Session storage key for pending /authorize → /callback round-trip data
 * (state, PKCE verifier, nonce, post-login redirect target).
 */
export const OAUTH_PENDING_SESSION_KEY = 'oauth_pending';

/**
 * Maximum number of pending authorization requests kept per browser session.
 *
 * This lets duplicate clicks, reloads, and multi-tab login attempts complete
 * independently without letting stale state accumulate in cookie-backed sessions.
 *
 * The value is bounded by the cookie-session byte budget, not by UX: browsers
 * drop any cookie whose name+value exceeds ~4093 bytes, and the post-login
 * session must fit `{oauth: tokens, oauth_pending: [...]}` in ONE encrypted
 * cookie. With production-size tokens (~1.8KB raw), each ~230-byte pending
 * costs ~330 encoded bytes, so 4 pendings keeps the worst case near 3.7KB.
 */
export const OAUTH_PENDING_SESSION_LIMIT = 4;

/**
 * Authenticated OAuth session.
 */
export interface OAuthSession {
  /** Token expiration timestamp in milliseconds since epoch. */
  expireAt: number;
  /** The current access token. */
  accessToken: string;
  /** Refresh token for obtaining new access tokens (when issued). */
  refreshToken?: string;
  /** OIDC id_token (when issued). */
  idToken?: string;
  /** URL to redirect to after a successful login. */
  redirectToAfterLogin?: string;
}

/**
 * State captured before redirecting to /authorize. Validated on callback.
 */
export interface OAuthPendingSession {
  /** CSRF token echoed by the authorization server. */
  state: string;
  /** PKCE verifier paired with the challenge sent to /authorize. */
  codeVerifier?: string;
  /** OIDC nonce sent to /authorize. */
  nonce?: string;
  /** Where to send the browser after a successful login. */
  redirectTo?: string;
  /** `redirect_uri` sent to /authorize; must be replayed verbatim at /token. */
  redirectUri?: string;
}

function isOAuthPendingSession(value: unknown): value is OAuthPendingSession {
  if (!value || typeof value !== 'object') return false;
  return typeof (value as { state?: unknown }).state === 'string';
}

/**
 * Normalizes pending OAuth session data.
 *
 * Older versions stored a single object under `oauth_pending`. New versions
 * store a bounded array, but callbacks should still accept pre-existing
 * sessions created before an upgrade.
 */
export function normalizeOAuthPendingSessions(value: unknown): OAuthPendingSession[] {
  if (Array.isArray(value)) return value.filter(isOAuthPendingSession);
  if (isOAuthPendingSession(value)) return [value];
  return [];
}

/**
 * Adds a pending OAuth authorization request and keeps only the newest entries.
 */
export function appendOAuthPendingSession(value: unknown, pending: OAuthPendingSession): OAuthPendingSession[] {
  const existing = normalizeOAuthPendingSessions(value).filter((entry) => entry.state !== pending.state);
  return [...existing, pending].slice(-OAUTH_PENDING_SESSION_LIMIT);
}
