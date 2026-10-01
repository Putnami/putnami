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

#### `migrations` (optional)

Associates migrations with the table. These migrations will be registered and can be executed automatically.

```typescript
const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `CREATE TABLE users (...)`,
};

const UsersTable = Table("users", {
  id:    Key(String),
  email: Column(String),
}, {
  migrations: [CreateUsersTable],
});
```

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

When `true`, the key will be auto-generated if not provided during save operations. This is typically used with UUID or serial columns.

```typescript
const UsersTable = Table("users", {
  id: Key(Uuid, { autoGenerate: true }), // Auto-generated if not provided
});
```

#### `columnType` (optional)

Explicit PostgreSQL column type override.

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

Explicit PostgreSQL column type. Useful for custom types or when automatic type inference is not sufficient.

```typescript
const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(String, { columnType: "VARCHAR(255)" }),
  score: Column(Number, { columnType: "NUMERIC(10,2)" }),
});
```

#### `default` (optional)

Default value for the column. Can be a literal value or a SQL expression.

```typescript
const UsersTable = Table("users", {
  id:        Key(Uuid, { autoGenerate: true }),
  status:    Column(String, { default: "active" }),
  createdAt: Column(DateIso, { default: "NOW()" }),
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
    fromDatabase: (value: string) => JSON.parse(value),
    toDatabase: (value: string[]) => JSON.stringify(value),
  }),
});
```

> **DateIso auto-coercion**: `DateIso` and `Optional(DateIso)` columns automatically convert JavaScript `Date` objects (returned by PostgreSQL for `TIMESTAMPTZ` columns) to ISO 8601 strings. No manual `fromDatabase`/`toDatabase` converters are needed:
>
> ```typescript
> createdAt: Column(DateIso, { columnName: "created_at" }), // Just works — Date → string
> ```

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

const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `
    CREATE TABLE users (
      id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
      email VARCHAR(255) UNIQUE NOT NULL,
      full_name VARCHAR(255) NOT NULL,
      bio TEXT,
      tags JSONB DEFAULT '[]',
      created_at TIMESTAMPTZ DEFAULT NOW(),
      updated_at TIMESTAMPTZ DEFAULT NOW()
    );
  `,
};

const UsersTable = Table("users", {
  id:        Key(Uuid, { autoGenerate: true }),
  email:     Column(Email),
  fullName:  Column(String, { columnName: "full_name" }),
  bio:       Column(Optional(String)),
  tags:      Column(String, {
    fromDatabase: (value: string) => JSON.parse(value),
    toDatabase: (value: string[]) => JSON.stringify(value),
  }),
  createdAt: Column(DateIso, { columnName: "created_at" }),
  updatedAt: Column(DateIso, { columnName: "updated_at" }),
}, {
  db: "main",
  migrations: [CreateUsersTable],
});

type User = InferTable<typeof UsersTable>;
```

### Best Practices

1. **Always define at least one `Key`** - Primary keys are required for repository operations
2. **Use descriptive table names** - Pass an explicit table name as the first argument to `Table`
3. **Use `Optional()` for nullable fields** - Wrap the schema primitive with `Optional()` instead of separate nullable options
4. **Group related configuration** - Use table-level options for `db`, `schema`, and `migrations`
5. **Document complex transformations** - Add comments for custom `toDatabase`/`fromDatabase` functions
6. **Leverage `InferTable`** - Derive entity types from the table definition instead of maintaining separate type declarations
