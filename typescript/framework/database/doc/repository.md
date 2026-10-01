# Repository Pattern

The Repository pattern provides a clean, type-safe interface for database operations. This guide covers all available methods and how to extend repositories with custom queries.

## Table of Contents

- [Basic Usage](#basic-usage)
- [Repository Methods](#repository-methods)
- [Custom Repositories](#custom-repositories)
- [Query Options](#query-options)
- [Filtering](#filtering)
- [Query Profiling](#query-profiling)
- [Best Practices](#best-practices)

## Basic Usage

Create a repository by extending the `Repository` class with a table definition:

```typescript
import { Table, Column, Key, Uuid, Email, DateIso, Repository } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:        Key(Uuid),
  email:     Column(Email),
  name:      Column(String),
  status:    Column(String),
  createdAt: Column(DateIso, { columnName: "created_at" }),
});

type User = InferTable<typeof UsersTable>;

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }
}

// Use the repository
const userRepo = new UserRepository();
```

> The examples below filter on `status` and order by `created_at`, both declared
> on `UsersTable` above. Filter keys are validated against the schema at runtime —
> an unknown property throws rather than reaching SQL.

## Repository Methods

### `exists(partials: Partial<ENTITY>): Promise<boolean>`

Check if an entity exists by its primary key(s).

```typescript
const exists = await userRepo.exists({ id: "user-123" });
// Returns: true or false
```

**Note:** Requires at least one key property to be provided.

### `get(keys: Partial<ENTITY>): Promise<ENTITY | undefined>`

Get an entity by its primary key(s). This is an alias for `findOne` with key filters.

```typescript
const user = await userRepo.get({ id: "user-123" });
// Returns: User | undefined
```

### `count(filters?: Partial<ENTITY> | QueryFilters<ENTITY>): Promise<number>`

Count entities matching the filters with `SELECT count(*)`. Use this alongside a paginated `find` instead of loading every row to measure a total.

```typescript
const total = await userRepo.count({ active: true });
// Returns: number
```

### `findOne(filters: Partial<ENTITY>): Promise<ENTITY | undefined>`

Find a single entity matching the provided filters.

```typescript
const user = await userRepo.findOne({ email: "john@example.com" });
// Returns: User | undefined
```

**Note:** Returns the first matching entity. Use `find` for multiple results.

### `find(filters: Partial<ENTITY>, options?: FindOptions): Promise<ENTITY[]>`

Find multiple entities matching the provided filters.

```typescript
// Basic find
const users = await userRepo.find({ status: "active" });

// With options
const users = await userRepo.find(
  { status: "active" },
  {
    limit: 10,
    orderBy: "created_at DESC",
  }
);
```

**Options:**
- `limit?: number` - Maximum number of results (default: 1000). Clamped to the configured `maxRowLimit` (default: 10000) so request-controlled input cannot ask for an unbounded result set.
- `offset?: number` - Number of results to skip (default: 0)
- `orderBy?: string` - SQL ORDER BY clause (e.g., `"created_at DESC"`)

**Example with pagination:**
```typescript
// First page (10 results)
const page1 = await userRepo.find(
  { status: "active" },
  { limit: 10, offset: 0, orderBy: "created_at DESC" }
);

// Second page
const page2 = await userRepo.find(
  { status: "active" },
  { limit: 10, offset: 10, orderBy: "created_at DESC" }
);
```

### `save(entity: Partial<ENTITY>, options?): Promise<ENTITY>`

Save an entity (insert or update on conflict). Uses `ON CONFLICT` to handle updates.

```typescript
// Insert new entity
const newUser = await userRepo.save({
  id: "user-123",
  email: "john@example.com",
  name: "John Doe",
});

// Update existing entity (same primary key)
const updated = await userRepo.save({
  id: "user-123",
  name: "John Smith", // Updated name
});
```

**Options:**

| Option | Type | Default | Description |
|---|---|---|---|
| `strict` | `boolean` | `false` | Validate that all required (non-optional) fields are present. Recommended for inserts. |

```typescript
// Strict validation — ensures all required fields are provided
const user = await userRepo.save({
  id: "user-123",
  email: "john@example.com",
  name: "John Doe",
}, { strict: true });

// Without strict, partial updates are fine (only validates provided fields)
const updated = await userRepo.save({
  id: "user-123",
  name: "John Smith",
});
```

**Behavior:**
- If the entity with the same primary key exists, it updates the non-key columns
- If the entity doesn't exist, it inserts a new row
- Returns the saved entity with all columns populated
- Entity data is validated against the table schema before saving

**Note:** Requires at least one key property to be provided.

### `delete(filters: Partial<ENTITY>): Promise<{ success: boolean; item?: ENTITY }>`

Delete an entity by its primary key(s). Only key columns are used to build the
`WHERE` clause; any non-key fields in `filters` are ignored.

```typescript
const result = await userRepo.delete({ id: "user-123" });
// Returns: { success: true, item: User } or { success: false }
```

**Returns:**
- `success: boolean` - Whether a matching row was deleted
- `item?: ENTITY` - The deleted entity (if it existed)

**Note:** Requires at least one primary-key value. Calling `delete({})` (or with
only non-key fields) throws `DELETE_WITHOUT_FILTERS` — symmetric with
`deleteMany`, which also refuses an empty filter — rather than silently deleting
nothing.

### `saveMany(entities: Partial<ENTITY>[]): Promise<ENTITY[]>`

Save multiple entities in bulk. Processes entities in batches of 100 for optimal performance.

```typescript
const users = await userRepo.saveMany([
  { id: "user-1", email: "user1@example.com", name: "User 1" },
  { id: "user-2", email: "user2@example.com", name: "User 2" },
  { id: "user-3", email: "user3@example.com", name: "User 3" },
]);
// Returns: User[]
```

**Behavior:**
- Processes entities in batches of 100
- Uses `save()` internally, so each entity follows the same insert/update logic
- All entities are saved in parallel within each batch
- Returns all saved entities

**Note:** Throws `RepositoryError` if the array is empty.

### `deleteMany(filters: Partial<ENTITY> | QueryFilters<ENTITY>): Promise<number>`

Delete multiple entities matching the provided filters.

```typescript
// Delete all inactive users
const deletedCount = await userRepo.deleteMany({ status: "inactive" });
// Returns: number (count of deleted entities)
```

**Behavior:**
- Requires filters for safety (prevents accidental deletion of all records)
- Returns the number of deleted entities
- Uses `DELETE ... RETURNING *` to count deleted rows

**Note:** Throws `RepositoryError` if no filters are provided.

### `conn(): Promise<SqlClient>`

Get the database connection for this repository (the putnami-owned `SqlClient` type — today an alias of postgres.js's `Sql`). Useful for custom queries.

```typescript
const sql = await userRepo.conn();

// Use for custom queries
const result = await sql`
  SELECT u.*, COUNT(o.id) as order_count
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
  WHERE u.id = ${userId}
  GROUP BY u.id
`;
```

## Custom Repositories

Extend repositories with custom methods for domain-specific queries:

```typescript
import { Table, Column, Key, Uuid, Email, Optional, DateIso, Repository } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:        Key(Uuid),
  email:     Column(Email),
  name:      Column(String),
  status:    Column(String),
  createdAt: Column(DateIso, { columnName: "created_at" }),
});

type User = InferTable<typeof UsersTable>;

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  // Find by email
  async findByEmail(email: string): Promise<User | undefined> {
    return this.findOne({ email });
  }

  // Find active users
  async findActiveUsers(limit = 10): Promise<User[]> {
    return this.find(
      { status: "active" },
      { limit, orderBy: "created_at DESC" }
    );
  }

  // Find users with custom query
  async findUsersWithOrders(): Promise<User[]> {
    const sql = await this.conn();
    const rows = await sql`
      SELECT DISTINCT u.*
      FROM users u
      INNER JOIN orders o ON o.user_id = u.id
      WHERE o.status = 'completed'
    `;
    return rows.map((row) => this.helper.toEntity<User>(row));
  }

  // Count entities
  async count(filters: Partial<User>): Promise<number> {
    const sql = await this.conn();
    const result = await sql`
      SELECT COUNT(*) as count
      FROM ${sql(this.helper.tableName)}
    `;
    return Number(result[0].count);
  }
}
```

**Note:** The `helper` property provides access to table metadata:
- `helper.tableName` - The table name
- `helper.db` - The datasource name
- `helper.toEntity<T>(row)` - Convert a database row to an entity
- `helper.extractKeys(partials)` - Extract key properties from partial entity

## Query Options

### Limit Results

```typescript
const users = await userRepo.find(
  { status: "active" },
  { limit: 5 }
);
```

### Order Results

```typescript
// Ascending
const users = await userRepo.find(
  {},
  { orderBy: "created_at ASC" }
);

// Descending
const users = await userRepo.find(
  {},
  { orderBy: "created_at DESC" }
);
```

**Note:** Property names in `orderBy` are validated against the table schema — unknown properties are silently ignored, preventing injection via user-controlled input.

### Filtering

#### Basic Filters

Filters support exact matches on any column:

```typescript
// Single filter
const users = await userRepo.find({ status: "active" });

// Multiple filters (AND)
const users = await userRepo.find({
  status: "active",
  role: "admin",
});

// Undefined values are ignored
const users = await userRepo.find({
  status: "active",
  role: undefined, // This filter is ignored
});
```

#### Query Operators

For advanced filtering, use Prisma-style query operators:

```typescript
import { QueryFilters } from "@putnami/database";

// Comparison operators
const users = await userRepo.find({
  age: { gt: 18 },
  score: { gte: 100 },
});

// Range (multiple operators on same field)
const users = await userRepo.find({
  price: { gte: 100, lte: 500 },
});

// in operator
const users = await userRepo.find({
  status: { in: ["active", "pending"] },
});

// like/ilike (case-insensitive)
const users = await userRepo.find({
  email: { ilike: "%@example.com" },
  name: { like: "John%" },
});

// NULL checks
const users = await userRepo.find({
  deletedAt: { isNull: true },
  email: { isNotNull: true },
});

// Not equal
const users = await userRepo.find({
  role: { not: "admin" },
});
```

**Available Operators:**

| Operator | Description |
|---|---|
| `equals` | Equal (explicit form of direct value) |
| `not` | Not equal |
| `gt` | Greater than |
| `lt` | Less than |
| `gte` | Greater than or equal |
| `lte` | Less than or equal |
| `in` | Value in array |
| `notIn` | Value not in array |
| `like` | Pattern match (case-sensitive) |
| `ilike` | Pattern match (case-insensitive) |
| `isNull` | Check for NULL (`true` to match) |
| `isNotNull` | Check for NOT NULL (`true` to match) |

#### Complex Filters ($or and $and)

Use `$or` and `$and` for complex logical conditions:

```typescript
import { QueryFilters } from "@putnami/database";

// OR conditions
const users = await userRepo.find({
  $or: [
    { status: "active" },
    { role: "admin" },
  ],
});

// AND conditions
const users = await userRepo.find({
  $and: [
    { status: "active" },
    { age: { gte: 18 } },
  ],
});

// Complex nested conditions
const users = await userRepo.find({
  $or: [
    { status: "active", role: "admin" },
    { status: "pending", role: "moderator" },
  ],
  age: { gte: 18 },
});
```

**Note:** When using `$or` or `$and`, you can combine them with regular filters. Regular filters are combined with AND logic.

## Error Handling

The repository uses custom error types for better error handling and debugging:

### RepositoryError

Thrown by repository operations when something goes wrong:

```typescript
import { RepositoryError } from "@putnami/database";

try {
  await userRepo.save({});
} catch (error) {
  if (error instanceof RepositoryError) {
    console.error(error.code);        // Error code (e.g., "EMPTY_ENTITY")
    console.error(error.message);     // Error message
    console.error(error.cause);       // Underlying error (if any)
  }
}
```

**Common Error Codes:**
- `EMPTY_ENTITY` - Attempted to save an empty entity
- `MISSING_PRIMARY_KEY` - Entity missing required primary key
- `VALIDATION_ERROR` - Entity data failed schema validation (e.g. missing required fields with `{ strict: true }`)
- `SAVE_ERROR` - Error during save operation
- `SAVE_FAILED` - Save operation returned no rows
- `DELETE_WITHOUT_FILTERS` - Attempted `delete` without a primary key, or `deleteMany` without filters
- `EMPTY_ENTITIES` - Attempted saveMany with empty array

### MigrationError

Thrown by migration operations:

```typescript
import { MigrationError } from "@putnami/database";

try {
  await migrationService.runMigrations();
} catch (error) {
  if (error instanceof MigrationError) {
    console.error(error.migration);   // Migration name
    console.error(error.message);     // Error message
    console.error(error.cause);       // Underlying error (if any)
  }
}
```

## Query Profiling

> **Note:** Query profiling has been superseded by the built-in [Observability](./observability.md) system. All repository operations now emit structured logs and telemetry metrics automatically, regardless of the `queryProfiling` flag.

### Configuration

```yaml
database:
  host: localhost
  database: myapp
  slowQueryThresholdMs: 200  # Warn on queries >= 200ms (0 = disabled)
```

### How It Works

Every repository operation (`find`, `save`, `delete`, `exists`, `saveMany`, `deleteMany`) is automatically instrumented:

- **Telemetry metrics** — counters for query count and errors, histograms for duration
- **Structured logs** — operation, table, duration, and row count at `debug` level
- **Slow query warnings** — `warn`-level logs when queries exceed `slowQueryThresholdMs`
- **Error tracking** — `error`-level logs with failure details

### Example Output

```text
[sql] find users { operation: "find", table: "users", duration: 12, rowCount: 42 }
[sql] save orders { operation: "save", table: "orders", duration: 8, rowCount: 1 }
[sql] Slow query: find users (312ms >= 200ms) { operation: "find", table: "users", duration: 312, thresholdMs: 200 }
[sql] save users failed { operation: "save", table: "users", duration: 45, error: "duplicate key..." }
```

### Telemetry Metrics

When the `telemetry()` plugin is active, the following metrics are emitted:

| Metric | Type | Description |
|---|---|---|
| `sql.{operation}.{table}` | Counter | Query count per operation and table |
| `sql.{operation}.{table}.duration` | Histogram | Query duration in ms |
| `sql.query.duration` | Histogram | Aggregate duration across all tables |
| `sql.query.error` | Counter | Aggregate error count |
| `sql.query.slow` | Counter | Queries exceeding the threshold |

See [Observability](./observability.md) for the full reference.

## Best Practices

### 1. Use Custom Repositories for Domain Logic

Keep domain-specific queries in custom repository methods:

```typescript
class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findActiveAdmins(): Promise<User[]> {
    return this.find(
      { status: "active", role: "admin" },
      { orderBy: "created_at DESC" }
    );
  }
}
```

### 2. Handle Optional Fields

Use TypeScript's optional chaining and nullish coalescing:

```typescript
const user = await userRepo.findOne({ email });
if (!user) {
  throw new Error("User not found");
}

const name = user.name ?? "Anonymous";
```

### 3. Use Transactions for Multiple Operations

For operations that need to be atomic, use `runInTransaction`:

```typescript
import { runInTransaction } from "@putnami/database";

await runInTransaction(async () => {
  await userRepo.save(user1);
  await userRepo.save(user2);
  // Both saves are atomic — commits on success, rolls back on error
});
```

See [Transactions](./transactions.md) for timeout options, manual primitives, and abort handling.

### 4. Leverage Type Safety

TypeScript provides compile-time type checking through `InferTable`:

```typescript
type User = InferTable<typeof UsersTable>;

// Type-safe
const user = await userRepo.get({ id: "123" });
if (user) {
  console.log(user.email); // TypeScript knows email exists
}
```

### 5. Handle Errors Gracefully

The repository throws custom error types for better error handling:

```typescript
import { RepositoryError } from "@putnami/database";

try {
  const user = await userRepo.save(newUser);
  return user;
} catch (error) {
  if (error instanceof RepositoryError) {
    // Handle repository-specific errors
    switch (error.code) {
      case "EMPTY_ENTITY":
        throw new Error("Cannot save empty entity");
      case "MISSING_PRIMARY_KEY":
        throw new Error("Primary key is required");
      case "SAVE_ERROR":
        // Check error.cause for the underlying error
        if (error.cause?.message.includes("unique constraint")) {
          throw new Error("Email already exists");
        }
        throw error;
      default:
        throw error;
    }
  }
  throw error;
}
```

**Error Types:**
- `RepositoryError` - Repository operation errors with `code` and optional `cause`
- `MigrationError` - Migration execution errors with `migration` name and optional `cause`

### 6. Use Raw SQL for Complex Queries

For complex queries that don't fit the repository pattern, use raw SQL:

```typescript
class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findUsersWithStats(): Promise<UserWithStats[]> {
    const sql = await this.conn();
    return await sql`
      SELECT
        u.*,
        COUNT(o.id) as order_count,
        SUM(o.total) as total_spent
      FROM users u
      LEFT JOIN orders o ON o.user_id = u.id
      GROUP BY u.id
      ORDER BY total_spent DESC
    `;
  }
}
```

## Common Patterns

### Pagination

The `find()` method supports `offset` for pagination:

```typescript
// Simple pagination
const page1 = await userRepo.find(
  { status: "active" },
  { limit: 10, offset: 0, orderBy: "created_at DESC" }
);

const page2 = await userRepo.find(
  { status: "active" },
  { limit: 10, offset: 10, orderBy: "created_at DESC" }
);
```

For a complete pagination helper with total count:

```typescript
class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findPaginated(
    filters: Partial<User>,
    page: number,
    pageSize: number = 10
  ): Promise<{ users: User[]; total: number }> {
    const offset = (page - 1) * pageSize;
    const sql = await this.conn();

    // Get total count
    const countResult = await sql`
      SELECT COUNT(*) as count
      FROM ${sql(this.helper.tableName)}
    `;
    const total = Number(countResult[0].count);

    // Get paginated results
    const users = await this.find(filters, {
      limit: pageSize,
      offset,
      orderBy: "created_at DESC",
    });

    return { users, total };
  }
}
```

### Soft Deletes

```typescript
import { Table, Column, Key, Uuid, Email, Optional, DateIso } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:        Key(Uuid),
  email:     Column(Email),
  deletedAt: Column(Optional(DateIso), { columnName: "deleted_at" }),
});

type User = InferTable<typeof UsersTable>;

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async softDelete(id: string): Promise<void> {
    await this.save({
      id,
      deletedAt: new Date().toISOString(),
    });
  }

  async findActive(): Promise<User[]> {
    return this.find({ deletedAt: undefined });
  }
}
```
