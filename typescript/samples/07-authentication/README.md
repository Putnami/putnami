# Authentication

OAuth2 + sessions + protected routes.

## Features

- OAuth2 authorization code flow via `oAuth2()` plugin
- Cookie-based encrypted sessions
- Auto-generated `/login` and `/logout` routes
- Protected API routes with `.secure()` middleware and `ctx.user`
- Optional authentication check with `accessToken()` utility
- Session inspection endpoint

## Prerequisites

**1. Session secrets** — Cookie encryption requires `session.cookieSecret`. The committed `conf/.env.test.yaml` ships an obvious test-only placeholder (never a real secret). For any other environment, generate a real value and supply it via the matching `conf/.env.<env>.yaml` (kept out of version control):

```bash
openssl rand -hex 32  # cookieSecret
```

**2. OAuth credentials** — Set via environment variables:

```bash
export OAUTH_CLIENT_ID=your-client-id
export OAUTH_CLIENT_SECRET=your-client-secret
```

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | `/login` | Start OAuth2 login flow (auto-generated) |
| GET | `/logout` | Logout and clear session (auto-generated) |
| GET | `/auth/session` | Inspect current session |
| GET | `/profile` | Protected: user profile |
| GET | `/protected` | Protected: example resource |
| GET | `/healthz` | Aggregate health check |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open your browser:

**1. Check session before login:**

```bash
curl http://localhost:3907/auth/session
```

Expected response:

```json
{
  "authenticated": false,
  "message": "Not logged in. Visit /login to authenticate."
}
```

**2. Try accessing a protected route without login:**

```bash
curl http://localhost:3907/profile
```

Expected response — HTTP 401 (the `.secure()` middleware rejects unauthenticated requests).

**3. Start the OAuth2 login flow:**

Open [http://localhost:3907/login](http://localhost:3907/login) in your browser.

Expected result — redirects to the OAuth2 provider's authorization page. After granting access, you are redirected back with an active session.

**4. Check session after login:**

```bash
curl --cookie cookies.txt http://localhost:3907/auth/session
```

Expected response:

```json
{
  "authenticated": true,
  "user": {
    "id": "...",
    "email": "user@example.com",
    "name": "User Name"
  }
}
```

**5. Access the protected profile:**

```bash
curl --cookie cookies.txt http://localhost:3907/profile
```

Expected response:

```json
{
  "profile": {
    "id": "...",
    "email": "user@example.com",
    "name": "User Name"
  }
}
```

**6. Logout:**

Open [http://localhost:3907/logout](http://localhost:3907/logout) in your browser.

Expected result — session is cleared, redirected to `/`.

## Test

```bash
putnami test .
```

## What this sample proves

The test drives the real OAuth/session plugin far enough to observe the public
security boundary without contacting the provider: `/login` produces an
authorization redirect, both protected endpoints return 401 without an
authenticated session, and the session inspection endpoint reports anonymous
state without disclosing credentials. Runtime startup also refuses missing
OAuth environment variables instead of substituting committed secrets.

OAuth identity, encrypted sessions, and API `.secure()` middleware are owned by
[`@putnami/application`](../../framework/application/README.md), outside the
`@putnami/web` feature. This sample therefore links to that owner rather than
inventing a parallel web authentication feature.
