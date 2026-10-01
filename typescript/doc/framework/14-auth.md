# Auth

Use the OAuth2 plugin in `@putnami/application` to enable authentication with external identity providers. Supports OAuth2/OIDC flows, token management, and session-based user state.

## Getting started

### Entry point

```ts
import { application, http, oAuth2 } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(oAuth2());
```

### Provider configuration

Configure your OAuth provider in `.env.local.yaml`:

```yaml
oauth:
  clientId: 'your-client-id'
  clientSecret: 'your-client-secret'
  authorizeUri: 'https://auth.example.com/authorize'
  tokenUri: 'https://auth.example.com/token'
  keysUri: 'https://auth.example.com/.well-known/jwks.json'
  userInfoUri: 'https://auth.example.com/userinfo'
  logoutUri: 'https://auth.example.com/logout'
  scopes:
    - 'openid'
    - 'profile'
    - 'email'
  callbackPath: '/auth/callback'
  loginPath: '/login'
  logoutPath: '/logout'
```

### Configuration options

```ts
oAuth2({
  // Custom routes (optional)
  loginPath: '/login',
  logoutPath: '/logout',
  callbackPath: '/auth/callback',

  // Post-auth redirect (optional)
  defaultRedirect: '/dashboard',
})
```

## Common providers

### Google

```yaml
oauth:
  clientId: 'your-google-client-id.apps.googleusercontent.com'
  clientSecret: 'your-google-client-secret'
  authorizeUri: 'https://accounts.google.com/o/oauth2/v2/auth'
  tokenUri: 'https://oauth2.googleapis.com/token'
  keysUri: 'https://www.googleapis.com/oauth2/v3/certs'
  userInfoUri: 'https://openidconnect.googleapis.com/v1/userinfo'
  scopes:
    - 'openid'
    - 'profile'
    - 'email'
```

### GitHub

```yaml
oauth:
  clientId: 'your-github-client-id'
  clientSecret: 'your-github-client-secret'
  authorizeUri: 'https://github.com/login/oauth/authorize'
  tokenUri: 'https://github.com/login/oauth/access_token'
  userInfoUri: 'https://api.github.com/user'
  scopes:
    - 'user:email'
    - 'read:user'
```

### Auth0

```yaml
oauth:
  clientId: 'your-auth0-client-id'
  clientSecret: 'your-auth0-client-secret'
  authorizeUri: 'https://your-tenant.auth0.com/authorize'
  tokenUri: 'https://your-tenant.auth0.com/oauth/token'
  keysUri: 'https://your-tenant.auth0.com/.well-known/jwks.json'
  userInfoUri: 'https://your-tenant.auth0.com/userinfo'
  logoutUri: 'https://your-tenant.auth0.com/v2/logout'
  scopes:
    - 'openid'
    - 'profile'
    - 'email'
```

### Keycloak

```yaml
oauth:
  clientId: 'your-keycloak-client'
  clientSecret: 'your-keycloak-secret'
  authorizeUri: 'https://keycloak.example.com/realms/myrealm/protocol/openid-connect/auth'
  tokenUri: 'https://keycloak.example.com/realms/myrealm/protocol/openid-connect/token'
  keysUri: 'https://keycloak.example.com/realms/myrealm/protocol/openid-connect/certs'
  userInfoUri: 'https://keycloak.example.com/realms/myrealm/protocol/openid-connect/userinfo'
  logoutUri: 'https://keycloak.example.com/realms/myrealm/protocol/openid-connect/logout'
  scopes:
    - 'openid'
    - 'profile'
    - 'email'
```

## Protecting routes

### API route protection

```ts
// src/app/api/profile/get.ts
import { endpoint, useUser, unauthorized } from '@putnami/application';

export default endpoint(async () => {
  const user = await useUser();

  if (!user) {
    return unauthorized('Authentication required');
  }

  return { user };
});
```

### Page loader protection

```ts
// src/app/dashboard/loader.ts
import { useUser, HttpResponse } from '@putnami/application';
import { loader } from '@putnami/web';

export default loader(async (ctx) => {
  const user = await useUser();

  if (!user) {
    // Redirect to login with return URL
    const returnUrl = encodeURIComponent(ctx.path());
    return HttpResponse.redirect(`/login?returnUrl=${returnUrl}`);
  }

  return { user };
});
```

### Layout-level protection

```ts
// src/app/dashboard/layout.loader.ts
import { useUser, HttpResponse } from '@putnami/application';
import { loader } from '@putnami/web';

export default loader(async () => {
  const user = await useUser();

  if (!user) {
    return HttpResponse.redirect('/login');
  }

  // All dashboard pages now have access to user
  return { user };
});
```

### Middleware-based protection

```ts
// src/lib/auth-middleware.ts
import type { HttpMiddleware } from '@putnami/application';
import { useUser, unauthorized, forbidden } from '@putnami/application';

export const requireAuth: HttpMiddleware = async (ctx, next) => {
  const user = await useUser();

  if (!user) {
    return unauthorized('Authentication required');
  }

  return next();
};

export const requireAdmin: HttpMiddleware = async (ctx, next) => {
  const user = await useUser();

  if (!user) {
    return unauthorized('Authentication required');
  }

  if (!user.roles?.includes('admin')) {
    return forbidden('Admin access required');
  }

  return next();
};
```

### Fluent route protection

Use the route builder’s `.secure()` helper as the preferred way to enforce route-level access rules.

```ts
// src/app/api/notes/get.ts
import { endpoint } from '@putnami/application';

export default endpoint()
  .secure({
    scopes: ['notes:read'],
    roles: ['admin'],
  })
  .handle(() => {
    return { ok: true };
  });
```

`.secure()` is the recommended way to enforce authentication and access rules on endpoints.

For APIs that should return `401` instead of redirecting to login, use `useUser({ redirect: false })`
or `accessToken({ redirect: false })` in custom middleware.

```ts
import { endpoint, useUser, unauthorized } from '@putnami/application';

export default endpoint(async () => {
  const user = await useUser({ redirect: false });
  if (!user) {
    return unauthorized('Authentication required');
  }
  return { ok: true };
});
```

## User information

### Getting the current user

```ts
import { endpoint, useUser } from '@putnami/application';

export default endpoint(async () => {
  const user = await useUser();

  if (user) {
    // User object contains claims from the ID token
    console.log(user.sub);      // Unique user ID
    console.log(user.email);    // Email address
    console.log(user.name);     // Display name
    console.log(user.picture);  // Profile picture URL
  }
});
```

### User type

```ts
interface OAuthUser {
  sub: string;           // Subject (unique ID)
  email?: string;        // Email address
  email_verified?: boolean;
  name?: string;         // Full name
  given_name?: string;   // First name
  family_name?: string;  // Last name
  picture?: string;      // Profile picture URL
  locale?: string;       // Preferred locale
  // Additional claims from your provider...
}
```

## OAuth service access

For advanced flows, access the active OAuth service with `useOAuthService()`:

```ts
import { endpoint, useOAuthService } from '@putnami/application';

export default endpoint(async () => {
  const oauth = useOAuthService();
  const token = await oauth.clientToken();
  return { hasToken: Boolean(token) };
});
```

`useOAuthService()` throws if the OAuth plugin is not active, so ensure your app is configured with `.use(oAuth2())`.

### Custom user enrichment

```ts
// src/lib/get-app-user.ts
import { useUser } from '@putnami/application';
import { UserRepository } from '../entities/user';

export async function getAppUser() {
  const oauthUser = await useUser();

  if (!oauthUser) {
    return null;
  }

  // Fetch or create local user record
  const userRepo = new UserRepository();
  let user = await userRepo.findOne({ oauthId: oauthUser.sub });

  if (!user) {
    // First login - create user
    user = await userRepo.save({
      id: crypto.randomUUID(),
      oauthId: oauthUser.sub,
      email: oauthUser.email,
      name: oauthUser.name,
      createdAt: new Date(),
    });
  }

  return user;
}
```

## Login and logout

### Login flow

The OAuth plugin automatically handles the login flow:

1. User visits `/login` (configurable)
2. Redirected to OAuth provider with state and PKCE
3. User authenticates with provider
4. Provider redirects to `/auth/callback` with code
5. Plugin exchanges code for tokens
6. User session is created
7. User redirected to original destination or default

### Triggering login

```tsx
// In a React component
import { Link } from '@putnami/web';

function LoginButton() {
  return <Link to="/login">Sign in</Link>;
}

// With return URL
function ProtectedLink() {
  return <Link to="/login?returnUrl=/dashboard">Access Dashboard</Link>;
}
```

### Logout flow

```tsx
import { Link } from '@putnami/web';

function LogoutButton() {
  return <Link to="/logout">Sign out</Link>;
}
```

### Programmatic logout

```ts
import { endpoint, HttpResponse } from '@putnami/application';

export default endpoint(async () => {
  // Clear session and redirect to logout
  return HttpResponse.redirect('/logout');
});
```

## Session configuration

OAuth tokens are stored in the session. Configure session storage:

```yaml
session:
  store: 'cookie'           # 'cookie', 'memory', or 'database'
  cookieName: 'session'
  cookieSecret: 'your-32-char-secret-key-here!!'
  ttl: 604800               # 7 days in seconds
  secure: true              # HTTPS only
  sameSite: 'lax'           # 'strict', 'lax', or 'none'
```

## Token management

### Accessing tokens

```ts
import { endpoint, getAccessToken, getIdToken } from '@putnami/application';

export default endpoint(async () => {
  const accessToken = await getAccessToken();
  const idToken = await getIdToken();

  // Use access token for API calls
  const response = await fetch('https://api.example.com/data', {
    headers: {
      Authorization: `Bearer ${accessToken}`,
    },
  });

  return response.json();
});
```

### Token refresh

Tokens are automatically refreshed when they expire (if refresh token is available).

## Security considerations

### CSRF protection

The OAuth plugin uses the `state` parameter to prevent CSRF attacks. Make sure your callback URL is properly configured.

### PKCE

PKCE (Proof Key for Code Exchange) is used by default for enhanced security in the authorization code flow.

### Secure cookies

Always use secure cookies in production:

```yaml
session:
  secure: true
  sameSite: 'lax'
```

### Environment variables

Never commit secrets. Use environment-specific configuration:

```yaml
# .env.local.yaml (development)
oauth:
  clientSecret: 'dev-secret'

# .env.production.yaml
oauth:
  clientSecret: ${OAUTH_CLIENT_SECRET}
```

## Related guides

- [Add authentication](/docs/how-to/add-authentication)
- [Sessions](/docs/frameworks/typescript/sessions)
