# Add authentication

You will add OAuth2 login and protect one route using `@putnami/application`.

## Steps

### 1) Enable OAuth2

Update your app entry, for example `apps/web/src/main.ts`:

```ts
import { application, http, oAuth2 } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(oAuth2());
```

### 2) Configure the provider

Create `.env.local.yaml`:

```yaml
oauth:
  clientId: 'your-client-id'
  clientSecret: 'your-client-secret'
  authorizeUri: 'https://auth.example.com/authorize'
  tokenUri: 'https://auth.example.com/token'
  keysUri: 'https://auth.example.com/.well-known/jwks.json'
  scopes:
    - 'openid'
    - 'profile'
    - 'email'
```

### 3) Protect one route

Create `apps/web/src/app/me/get.ts`:

```ts
import { endpoint, useUser, HttpResponse } from '@putnami/application';

export default endpoint(async () => {
  const user = await useUser();
  if (!user) {
    return HttpResponse.redirect('/login');
  }
  return { user };
});
```

Visit `/login` to sign in, then load `/me`.

## Result

You now have a protected route behind authentication.

> **Companion sample:** [`typescript/samples/07-authentication`](https://github.com/putnami/putnami/tree/main/typescript/samples/07-authentication) — a runnable project with OAuth2 flow, sessions, and protected routes. Run `putnami serve @example/authentication` from the workspace root.
