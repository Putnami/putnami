# Sessions

Session management with pluggable storage backends.

## Overview

The Application framework provides a flexible session management system with multiple storage backends: cookie-based, in-memory, and database-backed sessions.

## Basic Usage

```typescript
import { useSession, setSession, deleteSession } from '@putnami/application';

// In a route handler
export async function POST(ctx: HttpRequestContext) {
  // Store a value in the session
  setSession('userId', 123);
  
  // Retrieve a value
  const userId = useSession<number>('userId');
  
  // Delete a specific key
  deleteSession('userId');
  
  return { success: true };
}
```

## Configuration

Configure sessions via YAML:

```yaml
session:
  store: 'cookie'           # 'cookie' | 'memory' | 'database'
  cookieName: 'session'
  cookieSecret: '...'        # Required: crypto.randomBytes(32).toString('hex')
  ttl: 604800                # 1 week in seconds
```

### Generating Secrets

Generate secure secrets:

```bash
# Generate cookieSecret (32 bytes)
node -e "console.log(require('crypto').randomBytes(32).toString('hex'))"
```

## Store Types

### Cookie Store (Default)

Encrypted client-side storage. Data is encrypted and stored in cookies.

**Pros:**
- No server-side storage required
- Works across multiple servers (stateless)
- Automatic expiration

**Cons:**
- Limited size (4KB per cookie)
- Sent with every request

**Configuration:**

```yaml
session:
  store: 'cookie'
  cookieName: 'session'
  cookieSecret: 'your-32-byte-secret'
  ttl: 604800
```

### Memory Store

Server-side storage with TTL expiration. Data is stored in memory.

**Pros:**
- No size limits
- Fast access
- Automatic expiration

**Cons:**
- Lost on server restart
- Not shared across servers

**Configuration:**

```yaml
session:
  store: 'memory'
  ttl: 3600  # 1 hour
```

### Database Store

PostgreSQL-backed storage. Requires `@putnami/database` package.

**Pros:**
- Persistent across restarts
- Shared across servers
- No size limits

**Cons:**
- Requires database connection
- Slower than memory/cookie

**Setup:**

1. Install the SQL package:
```bash
putnami add @putnami/database
```

2. Import it (auto-registers the store):
```typescript
import '@putnami/database';
```

3. Configure:
```yaml
session:
  store: 'database'
database:
  host: 'localhost'
  database: 'myapp'
  user: 'postgres'
  password: 'password'
```

## Session API

### Setting Values

```typescript
import { setSession } from '@putnami/application';

// Set a single value
setSession('userId', 123);
setSession('username', 'john');
setSession('preferences', { theme: 'dark' });
```

### Getting Values

```typescript
import { useSession } from '@putnami/application';

// Get a value with type safety
const userId = useSession<number>('userId');
const username = useSession<string>('username');
const preferences = useSession<{ theme: string }>('preferences');
```

### Deleting Values

```typescript
import { deleteSession, deleteSessionAll } from '@putnami/application';

// Delete a specific key
deleteSession('userId');

// Clear entire session
deleteSessionAll();
```

### Getting All Session Data

```typescript
import { useSessionAll } from '@putnami/application';

interface SessionData {
  userId: number;
  username: string;
  preferences: { theme: string };
}

const session = useSessionAll<SessionData>();
```

### Setting All Session Data

```typescript
import { setSessionAll } from '@putnami/application';

setSessionAll({
  userId: 123,
  username: 'john',
  preferences: { theme: 'dark' },
});
```

### Checking Session Existence

```typescript
import { hasSession } from '@putnami/application';

if (hasSession('userId')) {
  const userId = useSession<number>('userId');
}
```

## Use Cases

### User Authentication

```typescript
// src/api/login/post.ts
import type { HttpRequestContext } from '@putnami/application';
import { setSession, unauthorized } from '@putnami/application';

export async function POST(ctx: HttpRequestContext) {
  const { email, password } = await ctx.body<{ email: string; password: string }>();

  // Verify credentials
  const user = await verifyUser(email, password);
  if (!user) {
    return unauthorized();
  }
  
  // Store user in session
  setSession('userId', user.id);
  setSession('email', user.email);
  
  return { success: true, user };
}
```

### Protected Routes

```typescript
// Middleware to check authentication
import type { HttpMiddleware } from '@putnami/application';
import { useSession, unauthorized } from '@putnami/application';

const authMiddleware: HttpMiddleware = async (ctx, next) => {
  const userId = useSession<number>('userId');

  if (!userId) {
    return unauthorized();
  }
  
  // Add user to context
  (ctx as any).user = { id: userId };
  
  return next();
};
```

### Shopping Cart

```typescript
// src/api/cart/get.ts
import type { HttpRequestContext } from '@putnami/application';
import { useSession } from '@putnami/application';

export function GET(ctx: HttpRequestContext) {
  const cart = useSession<Array<{ id: string; quantity: number }>>('cart') || [];
  return { cart };
}

// src/api/cart/post.ts
export async function POST(ctx: HttpRequestContext) {
  const { productId, quantity } = await ctx.body<{ productId: string; quantity: number }>();
  
  const cart = useSession<Array<{ id: string; quantity: number }>>('cart') || [];
  cart.push({ id: productId, quantity });
  
  setSession('cart', cart);
  
  return { cart };
}
```

## Custom Session Store

Create custom session stores by implementing `SessionStore`:

```typescript
import { registerSessionStore, type SessionStore } from '@putnami/application';

class RedisSessionStore implements SessionStore {
  constructor(private redis: RedisClient) {}

  get<T>(sessionId: string, key: string): T | undefined {
    const data = this.redis.get(`${sessionId}:${key}`);
    return data ? JSON.parse(data) : undefined;
  }

  set<T>(sessionId: string, key: string, value: T): void {
    this.redis.set(`${sessionId}:${key}`, JSON.stringify(value));
  }

  delete(sessionId: string, key: string): void {
    this.redis.del(`${sessionId}:${key}`);
  }

  getAll<S>(sessionId: string): S {
    const keys = this.redis.keys(`${sessionId}:*`);
    const data: any = {};
    for (const key of keys) {
      const value = this.redis.get(key);
      data[key.replace(`${sessionId}:`, '')] = JSON.parse(value);
    }
    return data as S;
  }

  setAll<S>(sessionId: string, session: S): void {
    for (const [key, value] of Object.entries(session)) {
      this.redis.set(`${sessionId}:${key}`, JSON.stringify(value));
    }
  }

  deleteAll(sessionId: string): void {
    this.redis.del(this.redis.keys(`${sessionId}:*`));
  }

  exists(sessionId: string): boolean {
    return this.redis.exists(sessionId) > 0;
  }
}

// Register the store
registerSessionStore('redis', () => new RedisSessionStore(redisClient));

// Use it
// session:
//   store: 'redis'
```

## Best Practices

1. **Use secure secrets** - Generate strong random secrets for cookie encryption
2. **Set appropriate TTL** - Balance security and user experience
3. **Don't store sensitive data** - Use sessions for identifiers, not secrets
4. **Clear sessions on logout** - Always call `deleteSessionAll()` on logout
5. **Choose the right store** - Cookie for stateless, memory for single-server, database for multi-server

## Next Steps

- Learn about [OAuth](oauth.md) for authentication flows
- Explore [HTTP Server](http-server.md) for middleware
- Check [API Reference](api-reference.md) for complete session API
