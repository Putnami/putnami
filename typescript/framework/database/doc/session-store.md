# Database Session Store

The Database Session Store provides a PostgreSQL-backed session storage solution for your application. Sessions are stored in a `sessions` table with automatic expiration and cleanup.

## Table of Contents

- [Overview](#overview)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Usage](#usage)
- [API Reference](#api-reference)
- [Automatic Cleanup](#automatic-cleanup)
- [Best Practices](#best-practices)

## Overview

The `DatabaseSessionStore` is a session store implementation that uses PostgreSQL to persist session data. It's ideal for:

- **Multi-instance deployments** - Sessions are shared across all instances
- **Persistent sessions** - Sessions survive server restarts
- **Queryable data** - Analyze sessions via SQL
- **Automatic expiration** - TTL-based session cleanup

### Features

- ✅ Persistent storage in PostgreSQL
- ✅ Automatic table creation
- ✅ TTL-based expiration
- ✅ JSONB data storage
- ✅ Automatic session touch (extends expiry on access)
- ✅ Indexed for performance
- ✅ Manual cleanup support

## Quick Start

### 1. Configure Session Store

In your application configuration:

```yaml
# .env.local.yaml
session:
  store: "database"
  ttl: 604800  # 1 week in seconds

database:
  host: localhost
  port: 5432
  database: myapp
  user: postgres
  password: password
```

### 2. Use in Your Application

The session store is automatically registered when you import `@putnami/database`:

```typescript
import "@putnami/database"; // Registers 'database' session store
import { createApp } from "@putnami/application";

const app = createApp({
  // Session store is automatically used based on config
});

app.listen(3000);
```

### 3. Use Sessions

```typescript
import { useSession } from "@putnami/application";

export async function handler(req: Request) {
  const session = useSession(req);

  // Set session data
  session.set("userId", "user-123");
  session.set("cart", { items: ["item-1", "item-2"] });

  // Get session data
  const userId = session.get<string>("userId");
  const cart = session.get<{ items: string[] }>("cart");

  return new Response("OK");
}
```

## Configuration

### Basic Configuration

```yaml
session:
  store: "database"
  ttl: 604800  # Session TTL in seconds (default: 1 week)
```

### Named Datasource

Use a specific database connection:

```yaml
session:
  store: "database"
  ttl: 604800

database:
  # Session store uses default connection
  host: localhost
  database: myapp

  # Or use a named datasource
  sessions:
    host: session-db.example.com
    database: sessions_db
```

**Programmatic configuration:**

```typescript
import { DatabaseSessionStore } from "@putnami/database";

// Use default database
const store = new DatabaseSessionStore();

// Use named datasource
const store = new DatabaseSessionStore("sessions");
```

### TTL Configuration

Configure session time-to-live:

```yaml
session:
  store: "database"
  ttl: 3600  # 1 hour
```

**Common TTL values:**
- `900` - 15 minutes (short-lived sessions)
- `3600` - 1 hour (typical web sessions)
- `86400` - 1 day
- `604800` - 1 week (default)
- `2592000` - 30 days (long-lived sessions)

## Usage

### Basic Session Operations

```typescript
import { useSession } from "@putnami/application";

export async function handler(req: Request) {
  const session = useSession(req);

  // Set values
  session.set("userId", "user-123");
  session.set("username", "john_doe");

  // Get values
  const userId = session.get<string>("userId");
  const username = session.get<string>("username");

  // Delete values
  session.delete("username");

  // Get all session data
  const allData = session.getAll();

  // Delete entire session
  session.deleteAll();

  return new Response("OK");
}
```

### Complex Data Types

The session store uses JSONB, so you can store complex objects:

```typescript
const session = useSession(req);

// Store objects
session.set("user", {
  id: "user-123",
  email: "john@example.com",
  preferences: {
    theme: "dark",
    language: "en",
  },
});

// Store arrays
session.set("cart", [
  { id: "item-1", quantity: 2 },
  { id: "item-2", quantity: 1 },
]);

// Retrieve with type safety
const user = session.get<{
  id: string;
  email: string;
  preferences: { theme: string; language: string };
}>("user");
```

### Async Operations

For direct access to the store (bypassing the session middleware):

```typescript
import { DatabaseSessionStore } from "@putnami/database";

const store = new DatabaseSessionStore();
await store.ensureTable(); // Ensure table exists

// Async get
const userId = await store.getAsync<string>("session-id", "userId");

// Async get all
const allData = await store.getAllAsync("session-id");

// Async set all
await store.setAllAsync("session-id", {
  userId: "user-123",
  cart: { items: [] },
});

// Async delete
await store.deleteAllAsync("session-id");

// Check existence
const exists = await store.existsAsync("session-id");
```

## API Reference

### DatabaseSessionStore

#### Constructor

```typescript
new DatabaseSessionStore(dbName?: string)
```

- `dbName?: string` - Optional named datasource (default: uses default database)

#### Methods

##### `ensureTable(): Promise<void>`

Ensures the `sessions` table exists. Called automatically on first use.

##### `get<T>(sessionId: string, key: string): T | undefined`

Synchronous get (uses sync cache). For async operations, use `getAsync`.

##### `set<T>(sessionId: string, key: string, value: T): void`

Synchronous set (uses fire-and-forget async). For async operations, use `setAllAsync`.

##### `delete(sessionId: string, key: string): void`

Synchronous delete (uses fire-and-forget async).

##### `getAll<S = Record<string, unknown>>(sessionId: string): S`

Synchronous get all (uses sync cache). For async operations, use `getAllAsync`.

##### `setAll<S>(sessionId: string, session: S): void`

Synchronous set all (uses fire-and-forget async). For async operations, use `setAllAsync`.

##### `deleteAll(sessionId: string): void`

Synchronous delete all (uses fire-and-forget async).

##### `exists(sessionId: string): boolean`

Synchronous exists check. For async operations, use `existsAsync`.

##### `getAsync<T>(sessionId: string, key: string): Promise<T | undefined>`

Async get a single value from the session.

##### `getAllAsync<S = Record<string, unknown>>(sessionId: string): Promise<S>`

Async get all session data. Automatically touches the session to extend expiry.

##### `setAllAsync<S>(sessionId: string, session: S): Promise<void>`

Async set all session data. Updates expiry time.

##### `deleteAllAsync(sessionId: string): Promise<void>`

Async delete entire session.

##### `existsAsync(sessionId: string): Promise<boolean>`

Async check if session exists and is not expired.

##### `cleanup(): Promise<number>`

Manually cleanup expired sessions. Returns the number of deleted sessions.

## Automatic Cleanup

### Session Expiration

Sessions are automatically expired based on TTL:

1. **On Access** - Expired sessions are not returned
2. **On Query** - `getAllAsync` filters out expired sessions
3. **Manual Cleanup** - Use `cleanup()` to remove expired sessions

### Manual Cleanup

Clean up expired sessions manually:

```typescript
import { DatabaseSessionStore } from "@putnami/database";

const store = new DatabaseSessionStore();
const deletedCount = await store.cleanup();
console.log(`Cleaned up ${deletedCount} expired sessions`);
```

### Scheduled Cleanup

Set up a scheduled job to clean up expired sessions:

```typescript
import { DatabaseSessionStore } from "@putnami/database";

const store = new DatabaseSessionStore();

// Run cleanup every hour
setInterval(async () => {
  const deleted = await store.cleanup();
  console.log(`Cleaned up ${deleted} expired sessions`);
}, 3600 * 1000);
```

## Database Schema

The session store automatically creates this table:

```sql
CREATE TABLE IF NOT EXISTS sessions (
  session_id VARCHAR(255) PRIMARY KEY,
  data JSONB NOT NULL DEFAULT '{}',
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires_at
ON sessions (expires_at);
```

### Schema Details

- **session_id** - Primary key, session identifier
- **data** - JSONB column storing session data
- **expires_at** - Timestamp when session expires
- **created_at** - When session was created
- **updated_at** - When session was last updated (touched)

## Best Practices

### 1. Choose Appropriate TTL

Match TTL to your use case:

```yaml
# Short-lived (e.g., OAuth flows)
session:
  ttl: 900  # 15 minutes

# Standard web sessions
session:
  ttl: 3600  # 1 hour

# Long-lived (e.g., "remember me")
session:
  ttl: 2592000  # 30 days
```

### 2. Store Minimal Data

Keep session data small and focused:

```typescript
// ✅ Good - Store identifiers only
session.set("userId", "user-123");
session.set("role", "admin");

// ❌ Bad - Don't store large objects
session.set("user", {
  // ... 100+ fields
});
```

### 3. Use Separate Stores for Different Purposes

Consider separate session stores for different concerns:

```yaml
# User sessions
session:
  store: "database"
  ttl: 3600

# Shopping cart (shorter TTL)
cart:
  store: "database"
  ttl: 1800  # 30 minutes
```

### 4. Monitor Session Table Size

Keep an eye on session table growth:

```sql
-- Check session count
SELECT COUNT(*) FROM sessions;

-- Check expired sessions
SELECT COUNT(*) FROM sessions WHERE expires_at < NOW();

-- Check table size
SELECT pg_size_pretty(pg_total_relation_size('sessions'));
```

### 5. Implement Cleanup Job

Set up regular cleanup to prevent table bloat:

```typescript
// In your application startup
import { DatabaseSessionStore } from "@putnami/database";

const store = new DatabaseSessionStore();

// Cleanup every 6 hours
setInterval(async () => {
  try {
    const deleted = await store.cleanup();
    console.log(`Cleaned up ${deleted} expired sessions`);
  } catch (error) {
    console.error("Session cleanup failed:", error);
  }
}, 6 * 3600 * 1000);
```

### 6. Handle Session Errors Gracefully

```typescript
try {
  const session = useSession(req);
  session.set("userId", userId);
} catch (error) {
  // Handle session store errors
  console.error("Session error:", error);
  // Fallback or retry logic
}
```

### 7. Use Type Safety

Leverage TypeScript for type-safe session access:

```typescript
interface UserSession {
  userId: string;
  role: "admin" | "user";
  preferences: {
    theme: "light" | "dark";
  };
}

const session = useSession(req);
const userSession = session.get<UserSession>("user");
if (userSession) {
  // TypeScript knows the structure
  console.log(userSession.userId);
}
```

### 8. Secure Session IDs

Use cryptographically secure session IDs:

```typescript
import { randomBytes } from "crypto";

const sessionId = randomBytes(32).toString("hex");
```

### 9. Monitor Performance

Watch for performance issues with large session tables:

```sql
-- Check for slow queries
EXPLAIN ANALYZE
SELECT * FROM sessions
WHERE session_id = '...' AND expires_at > NOW();
```

### 10. Use Indexes

The session store automatically creates an index on `expires_at`. For custom queries, add additional indexes:

```sql
-- Example: Index on created_at for analytics
CREATE INDEX IF NOT EXISTS idx_sessions_created_at
ON sessions (created_at);
```
