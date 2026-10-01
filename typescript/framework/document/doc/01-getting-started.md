# Getting Started with @putnami/document

`@putnami/document` adds portable document storage to Putnami applications. It gives you declarative collection definitions, typed repositories, cursor pagination, and pluggable adapters.

## Installation

```bash
bunx putnami deps add @putnami/document
```

Add the Firestore client only when you use the Firestore adapter:

```bash
bunx putnami deps add @google-cloud/firestore
```

## 1. Define a collection

```ts
import { Collection, DateIso, DocumentId, Email, Field, Optional } from '@putnami/document';

export const UsersCollection = Collection(
  'users',
  {
    id: DocumentId(String),
    email: Field(Email),
    name: Field(String),
    nickname: Field(Optional(String)),
    createdAt: Field(DateIso),
  },
  {
    indexes: [{ fields: ['email'] }],
  },
);
```

## 2. Create a repository

```ts
import { Repository } from '@putnami/document';
import { UsersCollection } from './users.collection';

export class UserRepository extends Repository<typeof UsersCollection> {
  constructor() {
    super(UsersCollection);
  }
}
```

## 3. Register the plugin

The `document()` plugin handles backend shutdown for cached adapters.

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () => application().use(document());
```

## 4. Configure a backend

For tests or lightweight local use:

```yaml
document:
  backend: memory
```

For Firestore:

```yaml
document:
  backend: firestore
  projectId: my-project
  databaseId: (default)
  emulatorHost: 127.0.0.1:8080
```

Named stores follow the `document.<name>` pattern:

```yaml
document:
  backend: memory

  audit:
    backend: firestore
    projectId: my-project
```

Then bind a collection to a named store with `db`:

```ts
export const AuditCollection = Collection(
  'audit',
  {
    id: DocumentId(String),
    action: Field(String),
  },
  { db: 'audit' },
);
```

## 5. Read and write documents

```ts
const repo = new UserRepository();

await repo.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice',
  createdAt: new Date().toISOString(),
});

const user = await repo.get('user-123');

const exists = await repo.exists('user-123');

const first = await repo.findOne({ email: 'alice@example.com' });
```

## 6. Query with portable filters

```ts
// Negative filters (`ne`, `notIn`, `exists: true`) are limited on Firestore:
// use at most one per query (split into multiple queries if you need more).
const result = await repo.find(
  {
    email: { notIn: ['blocked@example.com'] },
  },
  {
    limit: 25,
    orderBy: { field: 'email', direction: 'asc' },
  },
);
```

`find()` returns `{ items, nextCursor }`. Pass `nextCursor` back into a later `find()` call to fetch the next page.

## Save semantics

`save()` replaces the stored document by default:

```ts
await repo.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice Updated',
});
```

Use `merge: true` when you want patch-style behavior:

```ts
await repo.save(
  {
    id: 'user-123',
    nickname: 'ally',
  },
  { merge: true },
);
```

## Validation

Collection schemas use the same schema primitives as `@putnami/runtime` and `@putnami/database`. Repository writes validate against those schemas automatically.

- replace writes default to full validation
- merge writes default to partial validation
- pass `strict` explicitly when you want to override that default

## Next steps

- [Repository and queries](./02-repository.md)
- [Transactions](./03-transactions.md)
- [Adapters and config](./04-adapters-and-config.md)
