# Sessions

Sessions in `@putnami/application` provide stateful user data storage across requests with multiple backend options.

## Getting started

### Configuration

Configure sessions in `.env.local.yaml`:

```yaml
session:
  store: 'cookie'
  cookieName: 'session'
  cookieSecret: 'your-32-character-secret-key!!!'
  ttl: 604800
  secure: true
  sameSite: 'lax'
```

### Configuration options

- **store** - Storage backend: `'cookie'`, `'memory'`, or `'database'`
- **cookieName** - Name of the session cookie (default: `'session'`)
- **cookieSecret** - Encryption key (32+ characters, required)
- **ttl** - Session lifetime in seconds (default: 604800 = 7 days)
- **secure** - HTTPS-only cookies (default: `true` in production)
- **sameSite** - Cookie SameSite policy: `'strict'`, `'lax'`, or `'none'`
- **algorithm** - Encryption algorithm (default: `'aes-256-cbc'`)

## Basic usage

### Setting session data

```ts
import { endpoint, setSession } from '@putnami/application';

export default endpoint()
  .body({ userId: String })
  .handle(async (ctx) => {
    const body = await ctx.body();

    // Set session data
    await setSession({
      userId: body.userId,
      loginTime: Date.now(),
    });

    return { ok: true };
  });
```

### Getting session data

```ts
import { endpoint, useSession } from '@putnami/application';

export default endpoint(async () => {
  const session = await useSession<{
    userId: string;
    loginTime: number;
  }>();

  if (!session) {
    return { authenticated: false };
  }

  return {
    authenticated: true,
    userId: session.userId,
    loginTime: session.loginTime,
  };
});
```

### Deleting session data

```ts
import { endpoint, deleteSession } from '@putnami/application';

export default endpoint(async () => {
  // Clear the session (logout)
  await deleteSession();

  return { ok: true };
});
```

## Storage backends

### Cookie storage

Session data is encrypted and stored directly in the cookie. Best for small amounts of data.

```yaml
session:
  store: 'cookie'
  cookieSecret: 'your-32-character-secret-key!!!'
```

**Pros:**

- No server-side storage needed
- Works across multiple server instances
- Simple setup

**Cons:**

- Limited to ~4KB of data
- All data sent with every request
- Cannot invalidate sessions server-side

### Memory storage

Session data stored in server memory. Best for development or single-server deployments.

```yaml
session:
  store: 'memory'
  cookieSecret: 'your-32-character-secret-key!!!'
```

**Pros:**

- Fast access
- No external dependencies
- Unlimited data size

**Cons:**

- Lost on server restart
- Doesn't work with multiple server instances
- Memory usage grows with active sessions

### Database storage

Session data stored in PostgreSQL. Best for production with multiple server instances.

```yaml
session:
  store: 'database'
  cookieSecret: 'your-32-character-secret-key!!!'

database:
  host: localhost
  database: myapp
  user: postgres
  password: secret
```

**Pros:**

- Persists across restarts
- Works with multiple server instances
- Can invalidate sessions server-side
- Unlimited data size

**Cons:**

- Requires database setup
- Slightly slower than memory
- Need to clean up expired sessions

## Session patterns

### User authentication

```ts
// Login
export async function login(ctx: HttpRequestContext) {
  const { email, password } = await ctx.body<LoginInput>();

  const user = await validateCredentials(email, password);
  if (!user) {
    throw new UnauthorizedException('Invalid credentials');
  }

  await setSession({
    userId: user.id,
    email: user.email,
    roles: user.roles,
  });

  return { ok: true, user: { id: user.id, email: user.email } };
}

// Logout
export async function logout() {
  await deleteSession();
  return { ok: true };
}

// Get current user
export async function getCurrentUser() {
  const session = await useSession<UserSession>();

  if (!session) {
    return null;
  }

  return {
    userId: session.userId,
    email: session.email,
    roles: session.roles,
  };
}
```

### Session middleware

```ts
import type { HttpMiddleware } from '@putnami/application';
import { useSession, unauthorized, forbidden } from '@putnami/application';

interface UserSession {
  userId: string;
  email: string;
  roles: string[];
}

export const requireSession: HttpMiddleware = async (ctx, next) => {
  const session = await useSession<UserSession>();

  if (!session) {
    return unauthorized('Session required');
  }

  return next();
};

export const requireRole = (role: string): HttpMiddleware => {
  return async (ctx, next) => {
    const session = await useSession<UserSession>();

    if (!session) {
      return unauthorized('Session required');
    }

    if (!session.roles.includes(role)) {
      return forbidden(`Role '${role}' required`);
    }

    return next();
  };
};
```

### Flash messages

```ts
import { useSession, setSession } from '@putnami/application';

interface SessionWithFlash {
  flash?: {
    type: 'success' | 'error' | 'info';
    message: string;
  };
  // ... other session data
}

// Set a flash message
export async function setFlash(type: 'success' | 'error' | 'info', message: string) {
  const session = await useSession<SessionWithFlash>() || {};
  await setSession({
    ...session,
    flash: { type, message },
  });
}

// Get and clear flash message
export async function getFlash() {
  const session = await useSession<SessionWithFlash>();

  if (!session?.flash) {
    return null;
  }

  const flash = session.flash;

  // Clear the flash message
  await setSession({
    ...session,
    flash: undefined,
  });

  return flash;
}
```

### Session renewal

```ts
import { useSession, setSession } from '@putnami/application';

interface UserSession {
  userId: string;
  lastActivity: number;
}

const SESSION_TIMEOUT = 30 * 60 * 1000; // 30 minutes
const RENEWAL_THRESHOLD = 5 * 60 * 1000; // 5 minutes

export async function renewSessionIfNeeded() {
  const session = await useSession<UserSession>();

  if (!session) {
    return;
  }

  const now = Date.now();
  const timeSinceActivity = now - session.lastActivity;

  // Check if session expired
  if (timeSinceActivity > SESSION_TIMEOUT) {
    await deleteSession();
    throw new UnauthorizedException('Session expired');
  }

  // Renew if close to expiry
  if (timeSinceActivity > SESSION_TIMEOUT - RENEWAL_THRESHOLD) {
    await setSession({
      ...session,
      lastActivity: now,
    });
  }
}
```

## Security considerations

### Secret key management

Never commit session secrets. Use environment variables:

```yaml
session:
  cookieSecret: ${SESSION_SECRET}
```

Generate strong secrets:

```bash
# Generate a 32-character secret
openssl rand -base64 32
```

### Secure cookies

Always use secure settings in production:

```yaml
session:
  secure: true        # HTTPS only
  sameSite: 'lax'     # CSRF protection
```

### Session fixation

Regenerate session ID after authentication:

```ts
export async function login(ctx: HttpRequestContext) {
  const { email, password } = await ctx.body<LoginInput>();
  const user = await validateCredentials(email, password);

  // Clear any existing session first
  await deleteSession();

  // Create new session with user data
  await setSession({
    userId: user.id,
    createdAt: Date.now(),
  });

  return { ok: true };
}
```

### Data minimization

Only store necessary data in sessions:

```ts
// Good - minimal data
await setSession({
  userId: user.id,
});

// Bad - too much data
await setSession({
  userId: user.id,
  email: user.email,
  name: user.name,
  address: user.address,
  // ... lots of user data
});
```

## Related guides

- [Auth](/docs/frameworks/typescript/auth)
- [Configuration](/docs/frameworks/typescript/configuration)
