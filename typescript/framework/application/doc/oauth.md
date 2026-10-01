# OAuth2 / OpenID Connect

The OAuth plugin is a generic OAuth2 / OIDC client for confidential web apps. It implements the authorization-code flow with state, OIDC nonce, and optional PKCE, and works against any spec-compliant provider — Putnami Auth, Google, Auth0, Okta, GitHub, Keycloak, etc.

## Quick start

```ts
import { application, http, oAuth2 } from '@putnami/application';

application()
  .use(http({ port: 3000 }))
  .use(oAuth2());
```

This registers:

- `GET /login` — generates `state` (and PKCE / nonce when appropriate), adds it to the session's pending authorization requests, and 302s to the authorization server's `/authorize`.
- `GET /auth/callback` (or whatever `callbackRoute` you configure; defaults to `loginRoute` for back-compat single-handler flows) — validates `state`, exchanges the code at `/token`, and stores tokens in the session.
- `GET /logout` — clears the session and 302s to the provider's end-session endpoint.

Tokens live under the `oauth` session key (`accessToken`, `refreshToken`, `expireAt`, `idToken`). Pending authorization requests live under `oauth_pending` as a short bounded list, so overlapping login attempts from the same browser session can complete independently. A callback is accepted only when its `state` matches one of those server-generated pending states, and the matched state is consumed.

## Configuration

```yaml
oauth:
  clientId: my-app
  clientSecret: ${OAUTH_CLIENT_SECRET}

  # OIDC discovery: fills in authorize/token/userinfo/jwks/end_session
  # endpoints from /.well-known/openid-configuration unless overridden.
  discoveryUri: https://auth.example.com/.well-known/openid-configuration

  # Where the browser comes back to after /authorize. Use an absolute URL.
  # When unset, the plugin computes `${request.domain}${callbackRoute ?? loginRoute}`.
  redirectUri: https://app.example.com/auth/callback

  # Local routes. callbackRoute defaults to loginRoute (single-handler back-compat).
  loginRoute: /login
  callbackRoute: /auth/callback
  logoutRoute: /logout

  scopes: [openid, profile, email]

  # PKCE policy. `auto` enables PKCE when discovery advertises a challenge method
  # (or when no client_secret is configured). Use `always` / `never` to force.
  pkce: auto

  # `client_secret_post` (default), `client_secret_basic`, or `none` (public client).
  tokenEndpointAuthMethod: client_secret_post

  # JWT verification claims (optional).
  issuer: https://auth.example.com
  audience: my-app

  # Where to send the browser after a successful login when no `?redirect=` is set.
  defaultLoginRedirectUrl: /
```

### Explicit endpoint overrides

If you don't use discovery — or want to override a specific endpoint that discovery returned — set any of the URI fields directly:

```yaml
oauth:
  clientId: my-app
  clientSecret: ${OAUTH_CLIENT_SECRET}
  authorizeUri: https://auth.example.com/authorize
  tokenUri:     https://auth.example.com/token
  keysUri:      https://auth.example.com/.well-known/jwks.json
  userInfoUri:  https://auth.example.com/userinfo
  signoutUri:   https://auth.example.com/logout
  scopes: [openid, profile, email]
```

When no `discoveryUri` and no explicit URIs are set, the plugin falls back to Putnami Auth's defaults.

### Programmatic overrides

```ts
application().use(http()).use(oAuth2({
  loginRoute: '/auth/sign-in',
  logoutRoute: '/auth/sign-out',
  callbackRoute: '/auth/callback',
}));
```

## How the flow runs

1. Browser hits `GET /login` (optionally with `?redirect=/somewhere`).
2. Plugin generates `state` (CSRF), `code_verifier` + `code_challenge` (when PKCE is in effect), and `nonce` (when `openid` is in the scopes). All four are saved as a pending authorization request under the `oauth_pending` session key together with the post-login `redirectTo`.
3. Browser is 302'd to the authorization server's `/authorize` with `response_type=code`, `client_id`, `redirect_uri`, `scope`, `state`, `nonce`, `code_challenge` and `code_challenge_method`.
4. Authorization server hands the browser back to `callbackRoute` with `?code=…&state=…`.
5. Plugin reloads `oauth_pending`, finds and consumes the pending request with a matching `state`, rejects the request if no match exists (see [Callback failures](#callback-failures)), then POSTs to `/token` with `application/x-www-form-urlencoded` — `grant_type=authorization_code`, `code`, `redirect_uri`, `code_verifier` (PKCE), plus the configured client-auth method.
6. Plugin stores `access_token` / `refresh_token` / `id_token` / `expireAt` under `oauth`; other unmatched pending authorization requests remain available until matched or evicted by newer logins.
7. Browser is 302'd to the captured `redirectTo` (or `defaultLoginRedirectUrl`).

## Using tokens in handlers

### `accessToken(options?)`

```ts
import { accessToken } from '@putnami/application';

export async function GET() {
  const token = await accessToken({ redirect: false });
  if (!token) return unauthorized();
  const res = await fetch('https://api.example.com/me', {
    headers: { Authorization: `Bearer ${token}` },
  });
  return res.json();
}
```

Resolution order:

1. `Authorization: Bearer <token>` request header.
2. Session-stored access token, refreshed via `refresh_token` when expired.
3. `throw redirect(loginRoute)` — unless `{ redirect: false }`.

### `useUser<T>(options?)`

```ts
import { useUser } from '@putnami/application';

interface MyUser {
  uid: string;
  sub: string;
  email?: string;
  name?: string;
}

export async function GET() {
  const user = await useUser<MyUser>();
  return { user };
}
```

`useUser()` resolves an access token, then tries (in order):

1. **JWT verify** of the access token against the discovered `jwks_uri`.
2. **JWT verify** of the OIDC `id_token` stored in the session.
3. **Userinfo** call — only when a `userInfoUri` is configured or discovered, **and** neither `oauth.audience` nor `oauth.issuer` is set.

Whichever returns first wins.

When `oauth.audience` or `oauth.issuer` is configured, the access-token verify is bound to them and a failed verify is final: the userinfo endpoint only proves a token is *active* at the IdP (it does not check `aud`/`iss`), so falling back to it would let a token minted for a sibling service on the same IdP authenticate here. Providers that issue opaque (non-JWT) access tokens therefore need the userinfo fallback and must not set `audience`/`issuer`.

### `clientToken()`

```ts
import { clientToken } from '@putnami/application';

const token = await clientToken();
```

Returns a cached client-credentials access token for service-to-service calls.

### `useOAuthService()`

Escape hatch for direct access to the `OAuthService` (e.g., `service.exchangeCode(...)`, `service.refreshToken(...)`, `service.userInfo(...)`).

## Securing routes

```ts
import { endpoint } from '@putnami/application';

export default endpoint()
  .secure({ scopes: ['notes:write'], roles: ['admin'] })
  .handle(() => ({ ok: true }));
```

`.secure()` runs the global security middleware (which delegates to `useUser()` under the hood) and rejects with `401` / `403` based on the configured scopes and roles. See [security.md](security.md) for the full guard model.

## Provider recipes

### Putnami Auth (OIDC)

```yaml
oauth:
  clientId: my-app
  clientSecret: ${OAUTH_CLIENT_SECRET}
  discoveryUri: https://auth.putnami.cloud/.well-known/openid-configuration
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [openid, profile, email]
```

### Google

```yaml
oauth:
  clientId: ${GOOGLE_CLIENT_ID}
  clientSecret: ${GOOGLE_CLIENT_SECRET}
  discoveryUri: https://accounts.google.com/.well-known/openid-configuration
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [openid, profile, email]
```

### Auth0

```yaml
oauth:
  clientId: ${AUTH0_CLIENT_ID}
  clientSecret: ${AUTH0_CLIENT_SECRET}
  discoveryUri: https://YOUR_TENANT.auth0.com/.well-known/openid-configuration
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [openid, profile, email]
```

### Okta

```yaml
oauth:
  clientId: ${OKTA_CLIENT_ID}
  clientSecret: ${OKTA_CLIENT_SECRET}
  discoveryUri: https://YOUR_DOMAIN.okta.com/.well-known/openid-configuration
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [openid, profile, email]
```

### GitHub (OAuth2 only, no OIDC discovery)

```yaml
oauth:
  clientId: ${GITHUB_CLIENT_ID}
  clientSecret: ${GITHUB_CLIENT_SECRET}
  authorizeUri: https://github.com/login/oauth/authorize
  tokenUri:     https://github.com/login/oauth/access_token
  userInfoUri:  https://api.github.com/user
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [read:user, user:email]
  pkce: never
```

GitHub doesn't issue JWT access tokens — `useUser()` falls back to the `userInfoUri` to get the user.

### Public client (no client secret)

```yaml
oauth:
  clientId: ${PUBLIC_CLIENT_ID}
  discoveryUri: https://auth.example.com/.well-known/openid-configuration
  redirectUri:  https://app.example.com/auth/callback
  callbackRoute: /auth/callback
  scopes: [openid, profile]
  tokenEndpointAuthMethod: none   # implies PKCE on by default
```

## Post-login redirect

A `?redirect=/path` query parameter on `/login` is captured (only same-origin paths — `//attacker.example` is rejected) and used as the destination after a successful callback. When no `?redirect` is given, `defaultLoginRedirectUrl` is used.

## Callback failures

The callback never exchanges a code when its `state` matches no pending authorization request. This happens when the user goes Back and approves again, or submits the consent form twice: the first callback already consumed the `state`.

- When the session already holds a usable OAuth session (the access token has not expired, or a refresh token is present), the callback redirects to `defaultLoginRedirectUrl`.
- Otherwise it fails with `400 invalid_state`.

Every callback failure (`invalid_state`, a provider `error` such as `access_denied`, a missing code, a failed token exchange, an invalid `id_token`) keeps its status code and picks its format from the `Accept` header:

| Client | Response |
| --- | --- |
| Ranks `text/html` above `application/json` (a browser navigation) | A short HTML page that explains the failure and links to `loginRoute` ("Sign in again"). It shows the error code, escaped, and never the provider's `error_description`. |
| Anything else, including no `Accept`, `*/*` or `application/json` | `{"error": "…", "error_description": "…"}` |

Both formats send `Cache-Control: no-store`.

## Logout

`GET /logout` clears the local session and 302s to the provider's end-session endpoint (`signoutUri` or the discovery doc's `end_session_endpoint`). To redirect back to your app after global signout, set `postLogoutRedirectUrl`:

```yaml
oauth:
  postLogoutRedirectUrl: https://app.example.com/
```

## Best practices

- **HTTPS in production** — cookies that carry the session and tokens are flagged `Secure`; loading the app over HTTP will silently drop them.
- **Configure `issuer` and `audience`** when you trust the JWT for authorization decisions; the identity middleware validates them globally.
- **Prefer discovery over hand-curated URIs** — that way `jwks_uri` rotations and endpoint changes are picked up automatically.
- **Use the smallest scope set you need** — additional scopes show up in the consent UI and become part of the audit trail.

## See also

- [Sessions](sessions.md) — how the cookie session that backs OAuth state and tokens works.
- [Security](security.md) — guard model, `.secure()`, and global security middleware.
- [API reference](api-reference.md) — full type signatures for `oAuth2`, `OAuthConfig`, `OAuthService`, `accessToken`, `useUser`, `clientToken`, `useOAuthService`.
