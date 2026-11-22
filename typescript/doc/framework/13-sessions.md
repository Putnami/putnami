# Sessions

Sessions in `@putnami/application` provide stateful user data storage across requests with multiple backend options. The session helpers are synchronous and read the current request's session: `useSession(key)` and `setSession(key, value)` work on one key, `useSessionAll()` and `setSessionAll(data)` on the whole session, `deleteSession(key)` and `deleteSessionAll()` remove data.

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

export default endpoint(async (ctx) => {
  const body = await ctx.body<{ userId: string }>();
  if (!body) return ctx.throw(400, 'userId is required');

  // Set session data, one key at a time
  setSession('userId', body.userId);
  setSession('loginTime', Date.now());

  return { ok: true };
});
```

### Getting session data

```ts
import { endpoint, useSession, useSessionAll } from '@putnami/application';

export default endpoint(async () => {
  const userId = useSession<string>('userId');

  if (!userId) {
    return { authenticated: false };
  }

  // Or read the whole session at once
  const session = useSessionAll<{ userId: string; loginTime: number }>();

  return {
    authenticated: true,
    userId,
    loginTime: session.loginTime,
  };
});
```

### Deleting session data

```ts
import { endpoint, deleteSession, deleteSessionAll } from '@putnami/application';

export default endpoint(async () => {
  // Remove one key
  deleteSession('loginTime');

  // Clear the whole session (logout)
  deleteSessionAll();

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
  default:
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
import type { HttpRequestContext } from '@putnami/application';
import { deleteSessionAll, setSessionAll, unauthorized, useSessionAll } from '@putnami/application';

interface UserSession {
  userId: string;
  email: string;
  roles: string[];
}

// Login
export async function login(ctx: HttpRequestContext) {
  const input = await ctx.body<LoginInput>();
  if (!input) return ctx.throw(400, 'Credentials are required');

  const user = await validateCredentials(input.email, input.password);
  if (!user) {
    return unauthorized('Invalid credentials');
  }

  setSessionAll<UserSession>({
    userId: user.id,
    email: user.email,
    roles: user.roles,
  });

  return { ok: true, user: { id: user.id, email: user.email } };
}

// Logout
export async function logout() {
  deleteSessionAll();
  return { ok: true };
}

// Get current user
export async function getCurrentUser() {
  const session = useSessionAll<Partial<UserSession>>();

  if (!session.userId) {
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
  if (!useSession<string>('userId')) {
    return unauthorized('Session required');
  }

  return next();
};

export const requireRole = (role: string): HttpMiddleware => {
  return async (ctx, next) => {
    const roles = useSession<UserSession['roles']>('roles');

    if (!roles) {
      return unauthorized('Session required');
    }

    if (!roles.includes(role)) {
      return forbidden(`Role '${role}' required`);
    }

    return next();
  };
};
```

### Flash messages

```ts
import { deleteSession, setSession, useSession } from '@putnami/application';

interface Flash {
  type: 'success' | 'error' | 'info';
  message: string;
}

// Set a flash message
export function setFlash(type: Flash['type'], message: string) {
  setSession<Flash>('flash', { type, message });
}

// Get and clear flash message
export function getFlash(): Flash | null {
  const flash = useSession<Flash>('flash');

  if (!flash) {
    return null;
  }

  // Clear the flash message
  deleteSession('flash');

  return flash;
}
```

### Session renewal

```ts
import { deleteSessionAll, setSession, useSession } from '@putnami/application';
import { UnauthorizedException } from '@putnami/runtime';

const SESSION_TIMEOUT = 30 * 60 * 1000; // 30 minutes
const RENEWAL_THRESHOLD = 5 * 60 * 1000; // 5 minutes

export function renewSessionIfNeeded() {
  const lastActivity = useSession<number>('lastActivity');

  if (lastActivity === undefined) {
    return;
  }

  const now = Date.now();
  const timeSinceActivity = now - lastActivity;

  // Check if session expired
  if (timeSinceActivity > SESSION_TIMEOUT) {
    deleteSessionAll();
    throw new UnauthorizedException('Session expired');
  }

  // Renew if close to expiry
  if (timeSinceActivity > SESSION_TIMEOUT - RENEWAL_THRESHOLD) {
    setSession('lastActivity', now);
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
  const input = await ctx.body<LoginInput>();
  if (!input) return ctx.throw(400, 'Credentials are required');
  const user = await validateCredentials(input.email, input.password);
  if (!user) return unauthorized('Invalid credentials');

  // Clear any existing session first
  deleteSessionAll();

  // Create new session with user data
  setSessionAll({
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
setSession('userId', user.id);

// Bad - too much data
setSessionAll({
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
