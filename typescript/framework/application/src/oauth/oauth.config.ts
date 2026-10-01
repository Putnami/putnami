import { ArrayOf, Config, Default, Int, Optional } from '@putnami/runtime';

/**
 * Configuration for the OAuth2 / OpenID Connect plugin.
 *
 * The plugin implements the standard authorization-code flow for confidential
 * web clients. Discovery, PKCE, multiple token-endpoint auth methods and
 * separate login/callback routes are supported so the same plugin can talk to
 * Putnami Auth, Google, Auth0, Okta, GitHub, Keycloak, etc.
 *
 * @example Putnami Auth via OIDC discovery
 * ```yaml
 * oauth:
 *   clientId: my-app
 *   clientSecret: ${OAUTH_CLIENT_SECRET}
 *   discoveryUri: https://auth.putnami.cloud/.well-known/openid-configuration
 *   redirectUri: https://app.example.com/auth/callback
 *   callbackRoute: /auth/callback
 *   scopes: [openid, profile, email]
 * ```
 *
 * @example Manual endpoint overrides
 * ```yaml
 * oauth:
 *   clientId: my-app
 *   clientSecret: ${OAUTH_CLIENT_SECRET}
 *   authorizeUri: https://auth.example.com/authorize
 *   tokenUri: https://auth.example.com/token
 *   keysUri: https://auth.example.com/.well-known/jwks.json
 *   userInfoUri: https://auth.example.com/userinfo
 *   signoutUri: https://auth.example.com/logout
 * ```
 */
export const OAuthConfig = Config('oauth', {
  /** OAuth2 client identifier issued by the authorization server. */
  clientId: Optional(String),
  /** OAuth2 client secret. Omit for public clients (`tokenEndpointAuthMethod: 'none'`). */
  clientSecret: Optional(String),

  /** Local route that starts the login flow. Defaults to `/login`. */
  loginRoute: Default(String, '/login'),
  /** Local route that handles the authorization-server callback. Defaults to `loginRoute`. */
  callbackRoute: Optional(String),
  /** Local route that clears the session and redirects to `signoutUri`. */
  logoutRoute: Default(String, '/logout'),

  /**
   * Full URL of the OIDC discovery document (`/.well-known/openid-configuration`).
   * When set, endpoint URIs and JWKS are pulled from it unless explicitly overridden.
   */
  discoveryUri: Optional(String),

  /** Authorization endpoint. Overrides discovery. */
  authorizeUri: Optional(String),
  /** Token endpoint. Overrides discovery. */
  tokenUri: Optional(String),
  /** JWKS endpoint. Overrides discovery. */
  keysUri: Optional(String),
  /** Userinfo endpoint. Overrides discovery. Optional — only used as a fallback when JWT verify fails. */
  userInfoUri: Optional(String),
  /** Provider logout / end-session endpoint. Overrides discovery (`end_session_endpoint`). */
  signoutUri: Optional(String),

  /**
   * Absolute `redirect_uri` sent to the authorization server. When unset, the
   * plugin computes `${request.domain}${callbackRoute ?? loginRoute}` at request time.
   */
  redirectUri: Optional(String),

  /** Where to send the browser after a successful login when no `redirectToAfterLogin` was captured. */
  defaultLoginRedirectUrl: Default(String, '/'),
  /** Full URL appended to `signoutUri` as `redirect_url`. When unset, signout is called without a redirect. */
  postLogoutRedirectUrl: Optional(String),

  /** Scopes requested at /authorize. Joined with a single space. */
  scopes: Default(ArrayOf(String), []),

  /**
   * PKCE policy.
   * - `auto` (default): enable PKCE when the discovery document advertises `code_challenge_methods_supported`.
   * - `always`: always send `code_challenge`/`code_verifier`.
   * - `never`: never send PKCE.
   */
  pkce: Default(String, 'auto'),

  /**
   * How client credentials are presented to the token endpoint.
   * - `client_secret_post` (default): credentials in the form body.
   * - `client_secret_basic`: credentials in an `Authorization: Basic` header.
   * - `none`: no client authentication (public client; PKCE is required).
   */
  tokenEndpointAuthMethod: Default(String, 'client_secret_post'),

  /** Token endpoint request timeout in milliseconds. Defaults to 10s. */
  tokenRequestTimeoutMs: Default(Int, 10_000),

  /** Roles required by route guards. Surfaced through the security framework. */
  roles: Default(ArrayOf(String), []),
  /** Static refresh token, if your deployment provisions one out-of-band. */
  refreshToken: Optional(String),
  /** Expected issuer for JWT verification. Defaults to discovery's `issuer`. */
  issuer: Optional(String),
  /** Expected audience for JWT verification. */
  audience: Optional(String),
});
