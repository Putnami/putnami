# Add persistence

You will add persistent data to a Putnami app.

Putnami splits persistence by data shape:

- use `@putnami/database` for relational PostgreSQL data, joins, and migrations
- use `@putnami/document` for document-shaped NoSQL data with portable repositories and cursor pagination
- use `@putnami/storage` for files and blobs

This guide walks through the PostgreSQL path. If your feature is document-shaped instead, see the [Document storage reference](/docs/frameworks/typescript/document).

## Steps

### 1) Add SQL + configure a database

```bash
putnami deps add @putnami/database
putnami deps add @putnami/migration
```

Set the connection as one Postgres URL in `DATABASE_URL`, or commit it in
`conf/.env.local.yaml` under the `default` datasource:

```yaml
database:
  default:
    url: postgres://postgres:your_password@localhost:5432/myapp
```

Add `?sslmode=disable` for a server without TLS other than `localhost`. A
deploy target's binding wins over both. See
[Persistence](/docs/frameworks/typescript/persistence#database-configuration).

### 2) Define a table + repository

Create `apps/web/src/db/user.ts`:

```ts
import { Table, Column, Key, Uuid, Email, Repository } from '@putnami/database';
import type { InferTable } from '@putnami/database';

export const CreateUsersTable = {
  name: '20250101000000-create-users-table',
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY,
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL
    );
  `,
};

export const UsersTable = Table('users', {
  id:    Key(Uuid),
  email: Column(Email),
  name:  Column(String),
});

export type User = InferTable<typeof UsersTable>;

export class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }
}
```

### 3) Enable the SQL plugin

Update `apps/web/src/main.ts`:

A table definition creates no SQL: the migration owns the table. Contribute it from a plugin with `sqlSourceInline`, and let the SQL plugin apply it at startup:

```ts
import { application, type Plugin } from '@putnami/application';
import { sql, sqlSourceInline } from '@putnami/database';
import type { MigrationContributor } from '@putnami/migration';
import { CreateUsersTable } from './db/user';

const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({
      namespace: 'app',
      datasource: { name: 'default', schema: 'public' },
      definitions: [CreateUsersTable],
    }),
  ],
};

export const app = () => application().use(sql({ autoApply: true })).use(migrations);
```

### 4) Read and write from a route

Create `apps/web/src/app/users/post.ts`:

```ts
import { endpoint } from '@putnami/application';
import { UserRepository } from '../../db/user';

export default endpoint(async () => {
  const repo = new UserRepository();
  const user = await repo.save({
    id: crypto.randomUUID(),
    email: 'hello@example.com',
    name: 'Hello',
  });
  return { user };
});
```

## Result

You now have an app that persists data.

> **Companion sample:** [`typescript/samples/06-database`](https://github.com/putnami/putnami/tree/main/typescript/samples/06-database) — a runnable project with repository pattern, migrations, and transactions. Run `putnami serve @example/database` from the workspace root.

## Document storage quick start

If you need document storage instead of PostgreSQL, the equivalent setup uses `@putnami/document`:

```ts
import { Collection, DocumentId, Field, Repository } from '@putnami/document';

export const UsersCollection = Collection(
  'users',
  {
    id: DocumentId(String),
    email: Field(String),
    name: Field(String),
  },
  {
    indexes: [{ fields: ['email'] }],
  },
);

export class UserRepository extends Repository<typeof UsersCollection> {
  constructor() {
    super(UsersCollection);
  }
}
```

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () => application().use(document());
```

```yaml
document:
  backend: memory
```

You now have the matching document-storage path when relational tables are not the right fit.
