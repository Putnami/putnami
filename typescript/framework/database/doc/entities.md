# Table Definitions

Table definitions provide a declarative way to define database tables and their columns using a fluent schema-based API. This guide covers all available builders and their options.

## Table of Contents

- [Basic Table Definition](#basic-table-definition)
- [Table Builder](#table-builder)
- [Key Builder](#key-builder)
- [Column Builder](#column-builder)
- [Type Inference](#type-inference)
- [Advanced Configuration](#advanced-configuration)

## Basic Table Definition

A table is defined using the `Table`, `Key`, and `Column` builders with schema primitives for column types:

```typescript
import { Table, Column, Key } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:    Key(String),
  email: Column(String),
  name:  Column(String),
});

type User = InferTable<typeof UsersTable>;
```

Entities are plain objects whose types are inferred from the table definition. There are no classes or decorators involved.

## Table Builder

The `Table` builder configures table-level settings and defines the column schema.

### Basic Usage

```typescript
const UsersTable = Table("users", {
  id:    Key(String),
  email: Column(String),
  name:  Column(String),
});
```

### Options

#### `db` (optional)

Specifies a named datasource. When provided, the table will use the database connection configured at `database.{name}`.

```typescript
const UsersTable = Table("users", {
  id:    Key(String),
  email: Column(String),
}, {
  db: "auth", // Uses database.auth config
});
```

#### `schema` (optional)

Specifies the database schema for the table.

```typescript
const UsersTable = Table("users", {
  id:    Key(String),
  email: Column(String),
}, {
  schema: "public",
});
```

A table definition creates no SQL and carries no migration. Declare the table
in a migration source; see [Migrations](./migrations.md).

## Key Builder

The `Key` builder marks a column as a primary key. A table must have at least one key column.

### Basic Usage

```typescript
const UsersTable = Table("users", {
  id: Key(String),
});
```

### Schema Primitives

The first argument to `Key` is a schema primitive from `@putnami/runtime`:

```typescript
import { Table, Key, Uuid, Int } from "@putnami/database";

const UsersTable = Table("users", {
  id: Key(Uuid), // UUID primary key
});

const CountersTable = Table("counters", {
  id: Key(Int), // Integer primary key
});
```

### Options

#### `columnName` (optional)

Custom column name for the key. If omitted, uses the property name.

```typescript
const UsersTable = Table("users", {
  id: Key(Uuid, { columnName: "user_id" }), // Maps to "user_id" column
});
```

#### `autoGenerate` (optional)

When `true`, the key may be absent when `save(..., { strict: true })` validates
an entity. The framework generates no value: `save()` without a key throws
`MISSING_PRIMARY_KEY`. Generate the id in code (`crypto.randomUUID()`), or
insert with SQL and let the column default (`DEFAULT gen_random_uuid()`) fill it.

```typescript
const UsersTable = Table("users", {
  id: Key(Uuid, { autoGenerate: true }), // the migration declares DEFAULT gen_random_uuid()
});
```

#### `columnType` (optional)

Documents the PostgreSQL type. The framework never reads it and generates no
DDL: the migration declares the type.

```typescript
const UsersTable = Table("users", {
  id: Key(String, { columnType: "VARCHAR(36)" }),
});
```

### Composite Keys

You can define multiple keys for composite primary keys:

```typescript
const UserRolesTable = Table("user_roles", {
  userId:     Key(Uuid),
  roleId:     Key(Uuid),
  assignedAt: Column(DateIso),
});

type UserRole = InferTable<typeof UserRolesTable>;
```

## Column Builder

The `Column` builder defines a database column with a schema primitive type.

### Basic Usage

```typescript
import { Table, Column, Key, Email } from "@putnami/database";

const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
  name:  Column(String),
});
```

### Schema Primitives

Use schema primitives from `@putnami/runtime` for column types:

- `String` - Text columns
- `Number` - Numeric columns
- `Int` - Integer columns
- `Uuid` - UUID columns
- `Email` - Email strings
- `DateIso` - ISO date strings
- `Boolean` - Boolean columns
- `Optional(Type)` - Nullable columns

### Options

#### `columnName` (optional)

Custom column name. If omitted, uses the property name.

```typescript
const UsersTable = Table("users", {
  id:       Key(Uuid),
  fullName: Column(String, { columnName: "full_name" }), // Maps to "full_name" column
});
```

#### `columnType` (optional)

Documents the PostgreSQL type. The framework never reads it and generates no
DDL: the migration declares the type.

```typescript
const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(String, { columnType: "VARCHAR(255)" }),
  score: Column(Number, { columnType: "NUMERIC(10,2)" }),
});
```

#### `default` (optional)

Marks the field as optional when `save(..., { strict: true })` validates an
entity. The value is never sent to SQL: declare the default in the migration
(`status TEXT NOT NULL DEFAULT 'active'`, `created_at TIMESTAMPTZ DEFAULT now()`).

```typescript
const UsersTable = Table("users", {
  id:        Key(Uuid, { autoGenerate: true }),
  status:    Column(String, { default: "active" }),
  createdAt: Column(DateIso, { columnName: "created_at", default: "now()" }),
});
```

#### `toDatabase` (optional)

Custom transformation function to convert the property value before saving to the database.

```typescript
const UsersTable = Table("users", {
  id:   Key(Uuid),
  tags: Column(String, {
    toDatabase: (value: string[]) => JSON.stringify(value),
  }),
});
```

#### `fromDatabase` (optional)

Custom transformation function to convert the database value when reading from the database.

```typescript
const UsersTable = Table("users", {
  id:   Key(Uuid),
  tags: Column(String, {
    fromDatabase: (value) => JSON.parse(String(value)),
    toDatabase: (value: string[]) => JSON.stringify(value),
  }),
});
```

> **DateIso auto-coercion**: `DateIso` and `Optional(DateIso)` columns automatically convert JavaScript `Date` objects (returned by PostgreSQL for `TIMESTAMPTZ` columns) to ISO 8601 strings. No manual `fromDatabase`/`toDatabase` converters are needed:
>
> ```typescript
> createdAt: Column(DateIso, { columnName: "created_at" }), // Just works — Date → string
> ```

### Date and Time Columns

A date or time column reads back as the instant that was written, whatever the
time zone of the process:

| PostgreSQL type | Read as | Written by the framework as |
| --- | --- | --- |
| `DATE` | a `Date` at UTC midnight | the UTC calendar date, or a zone-less text as written |
| `TIMESTAMP` (without time zone) | a `Date` that takes the stored clock as UTC | the UTC wall clock, or a zone-less text as written |
| `TIMESTAMPTZ` | a `Date` at the stored instant | the instant, in UTC |

A `TIMESTAMP` holds a wall clock and no zone, so the framework takes that clock
as UTC when it writes and when it reads: a laptop with `TZ=Europe/Paris` and a
container in UTC read the same instant. The Go adapter
(`go.putnami.dev/database`, through pgx) reads `TIMESTAMP` as UTC too. A
`DateIso` column receives that instant as an ISO 8601 string ending in `Z`, and
a `DateIso` string saved to a `TIMESTAMP` column is written as its UTC clock.

A string bound to a `DATE` or `TIMESTAMP` column or cast is written by the same
serializer. A string without a zone in the ISO form
`YYYY-MM-DD[ HH:MM[:SS[.ffffff]]][ BC]`, a `T` allowed in place of the space,
names a date or a wall clock rather than an instant: it goes to PostgreSQL as
written, which stores that date or clock whatever the time zone of the process,
microseconds and BC years included. Bound to a `TIMESTAMP`,
`2026-11-22 10:30:00` stores `10:30:00`; bound to a `DATE`, `2026-11-22 00:00`
stores `2026-11-22`. A string with a zone, such as `2026-11-22T10:30:00Z` or
`2026-11-22T11:30:00+01:00`, is an instant and is written as its UTC clock or
date. Any other string is parsed as a JavaScript `Date`, which takes a text
without a zone as local time: give it a zone or use the ISO form.

A bare `Date` bound in a raw query is sent as `TIMESTAMPTZ`, and PostgreSQL
converts it to a `TIMESTAMP` column's clock in the session's `TimeZone`
setting, UTC unless the server or the role sets another. Prefer `TIMESTAMPTZ`
for a new column, which stores the instant itself.

`infinity` and `-infinity` have no JavaScript `Date`: in any of these columns
they read as an Invalid Date.

### Optional Columns

Use `Optional()` to mark a column as nullable:

```typescript
import { Table, Column, Key, Uuid, Optional } from "@putnami/database";

const UsersTable = Table("users", {
  id:  Key(Uuid),
  bio: Column(Optional(String)), // Nullable column
});

type User = InferTable<typeof UsersTable>;
// User.bio is string | undefined
```

## Type Inference

Types are inferred from the table definition using `InferTable`. There is no need to define a separate class or interface:

```typescript
import { Table, Column, Key, Uuid, Email, Optional } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:    Key(Uuid, { autoGenerate: true }),
  email: Column(Email),
  name:  Column(String),
  bio:   Column(Optional(String)),
});

type User = InferTable<typeof UsersTable>;
// Equivalent to:
// {
//   id: string;
//   email: string;
//   name: string;
//   bio?: string | undefined;
// }

// TypeScript knows the structure
const user: User = {
  id: "550e8400-e29b-41d4-a716-446655440000",
  email: "john@example.com",
  name: "John Doe",
};
```

## Advanced Configuration

### Complete Example

Here is a complete example showcasing all features:

```typescript
import { Table, Column, Key, Uuid, Email, Optional, DateIso } from "@putnami/database";
import type { InferTable } from "@putnami/database";

// The migration that creates the table:
//   CREATE TABLE users (
//     id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
//     email VARCHAR(255) UNIQUE NOT NULL,
//     full_name VARCHAR(255) NOT NULL,
//     bio TEXT,
//     tags JSONB DEFAULT '[]',
//     created_at TIMESTAMPTZ DEFAULT NOW(),
//     updated_at TIMESTAMPTZ DEFAULT NOW()
//   );

const UsersTable = Table("users", {
  id:        Key(Uuid, { autoGenerate: true }),
  email:     Column(Email),
  fullName:  Column(String, { columnName: "full_name" }),
  bio:       Column(Optional(String)),
  tags:      Column(String, {
    fromDatabase: (value) => JSON.parse(String(value)),
    toDatabase: (value: string[]) => JSON.stringify(value),
  }),
  createdAt: Column(DateIso, { columnName: "created_at" }),
  updatedAt: Column(DateIso, { columnName: "updated_at" }),
}, {
  db: "main",
});

type User = InferTable<typeof UsersTable>;
```

### Best Practices

1. **Always define at least one `Key`** - Primary keys are required for repository operations
2. **Use descriptive table names** - Pass an explicit table name as the first argument to `Table`
3. **Use `Optional()` for nullable fields** - Wrap the schema primitive with `Optional()` instead of separate nullable options
4. **Group related configuration** - Use table-level options for `db` and `schema`
5. **Document complex transformations** - Add comments for custom `toDatabase`/`fromDatabase` functions
6. **Leverage `InferTable`** - Derive entity types from the table definition instead of maintaining separate type declarations
