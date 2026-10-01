# @putnami/document

Portable document storage for Putnami.

Use this package when data is document-shaped and you want:

- declarative schemas via `Collection()`, `Field()`, and `DocumentId()`
- typed repositories
- cursor-based pagination
- portable query operators
- pluggable adapters

Do not use this package for:

- relational PostgreSQL data, joins, or migrations: use `@putnami/database`
- files and blobs: use `@putnami/storage`
- backend-native advanced features like full-text, regex, aggregation pipelines, subcollections, or realtime listeners

## Setup

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () =>
  application()
    .use(document());
```

Configuration in `conf/.env.local.yaml`:

```yaml
document:
  backend: memory
```

Named stores use `document.<name>`:

```yaml
document:
  backend: memory

  audit:
    backend: firestore
    projectId: my-project
    emulatorHost: 127.0.0.1:8080
```

## Collection Definitions

Collections are declarative and type-safe.

```ts
import { Collection, DateIso, DocumentId, Email, Field, Optional } from '@putnami/document';
import type { InferCollection, InferDocumentId } from '@putnami/document';

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

type User = InferCollection<typeof UsersCollection>;
type UserId = InferDocumentId<typeof UsersCollection>;
```

### Field Builders

| Builder | Purpose |
|---------|---------|
| `DocumentId(type)` | Document id field |
| `Field(type)` | Regular field |

### Field Options

```ts
Field(String, {
  fieldName: 'display_name',
  default: 'anonymous',
  toDocument: (value) => value,
  fromDocument: (value) => value,
});
```

- `fieldName` maps a property to a backend field name
- `default` affects full-document validation
- `toDocument` transforms values before storage
- `fromDocument` transforms values after reads

### Index Metadata

Collections can declare portable index coverage:

```ts
Collection(
  'users',
  {
    id: DocumentId(String),
    email: Field(String),
    createdAt: Field(DateIso),
  },
  {
    indexes: [
      { fields: ['email'] },
      { partition: ['email'], sort: ['createdAt'] },
    ],
  },
);
```

This is used for strict index validation and future partitioned adapters.

## Repository

Repositories provide typed CRUD around a collection:

```ts
import { Repository } from '@putnami/document';

export class UserRepository extends Repository<typeof UsersCollection> {
  constructor() {
    super(UsersCollection);
  }

  async findByEmail(email: string) {
    return this.findOne({ email });
  }
}
```

### Built-in Methods

| Method | Returns | Notes |
|--------|---------|-------|
| `get(id, opts?)` | `T \| undefined` | Read by document id |
| `exists(id, opts?)` | `boolean` | Existence check |
| `findOne(filters, opts?)` | `T \| undefined` | First matching document |
| `find(filters, opts?)` | `{ items: T[]; nextCursor?: string }` | Cursor-based pagination |
| `save(doc, opts?)` | `T` | Replace by default |
| `saveMany(docs)` | `T[]` | Replace semantics, full validation |
| `delete(id)` | `{ success: boolean; item?: T }` | Returns deleted item when present |
| `deleteMany(filters)` | `number` | Requires filters for safety |

## Query Model

Portable operators only:

```ts
await repo.find({ email: 'alice@example.com' });
await repo.find({ age: { gte: 18, lt: 65 } });
await repo.find({ email: { in: ['a@example.com', 'b@example.com'] } });
await repo.find({ tags: { contains: 'vip' } });
await repo.find({ nickname: { exists: false } });
```

Supported operators:

- `equals`
- `not`
- `gt`
- `gte`
- `lt`
- `lte`
- `in`
- `notIn`
- `contains`
- `exists`

Not supported in the public API:

- regex / like / ilike
- full-text
- geoqueries
- aggregation pipelines
- backend-native query DSLs

### Find Options

```ts
const page = await repo.find(
  { email: { not: 'blocked@example.com' } },
  {
    limit: 20,
    cursor: previousPage.nextCursor,
    orderBy: { field: 'createdAt', direction: 'desc' },
    consistency: 'strong',
  },
);
```

Important constraints:

- `orderBy` is structured, not SQL text
- pagination is cursor-based only
- there is no offset API
- treat `nextCursor` as opaque

## Save Semantics

`save()` is replace-by-default:

```ts
await repo.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice',
});
```

Use `{ merge: true }` for patch-style writes:

```ts
await repo.save(
  {
    id: 'user-123',
    nickname: 'ally',
  },
  { merge: true },
);
```

Validation behavior:

- replace writes default to full validation
- merge writes default to partial validation
- `saveMany()` always does full validation and replace writes in v1

Return value contract:

- `save()` and `saveMany()` return the **stored** document (read back after the
  write), not the request body. Any server-applied write transform is therefore
  reflected identically across adapters. This is portable behavior, not a
  backend quirk — every adapter must re-read after writing rather than echo the
  input. (`find` with `limit: 0` likewise returns zero rows, never an unbounded
  scan.)

## Transactions

Transactions are exposed as a package contract, not as backend-specific objects:

```ts
import { runInTransaction } from '@putnami/document';

await runInTransaction(async () => {
  await repo.save({ id: 'user-1', email: 'a@example.com', name: 'Alice' });
  await repo.save({ id: 'user-2', email: 'b@example.com', name: 'Bob' });
});
```

Rules:

- scoped to a single named store
- repository calls inside the transaction reuse the active transaction automatically
- cross-store access throws `TransactionNotSupported`
- transaction support depends on adapter capabilities

## Backends

### Memory

- good for tests and lightweight local use
- supports transactions
- supports composite document ids

### Firestore

- optional peer dependency loaded dynamically
- install `@google-cloud/firestore` only when needed
- supports transactions and batch writes
- currently requires a single scalar document id
- negative filters have Firestore-specific limitations; incompatible combinations throw `DocumentError`

## Consistency

Reads accept:

```ts
await repo.get('user-123', { consistency: 'strong' });
await repo.find({}, { consistency: 'eventual' });
```

Current adapters accept both values. The API stays explicit so future adapters can reject unsupported strong reads clearly.

## Strict Index Mode

Enable strict index enforcement:

```yaml
document:
  strictIndexes: true
```

When enabled, queries not covered by declared index metadata throw `IndexMissing`.

## Common Pitfalls

- Do not generate SQL-style `orderBy: 'created_at DESC'`; use structured `orderBy`
- Do not generate offset pagination
- Do not assume `save()` is partial update; it replaces unless `{ merge: true }`
- Do not call `deleteMany({})`; it throws for safety
- Do not rely on backend-specific document types leaking through the repository API
- Do not assume composite ids work on every adapter; Firestore currently rejects them
- Do not use this package for advanced Firestore or Mongo-specific queries; drop to the native client when needed

## DI Usage

Repositories can be provided like any other service:

```ts
application()
  .use(document())
  .provide(UserRepository)
  .provide(UserService, { deps: [UserRepository] });
```

## Detailed Documentation

See `README.md` and `doc/`:

- `doc/01-getting-started.md`
- `doc/02-repository.md`
- `doc/03-transactions.md`
- `doc/04-adapters-and-config.md`

## Support and feature ownership

`@putnami/document` is classified `stable` in the workspace support catalog and
owns the modeled [`typescript/document-repository`](putnami.features.json)
feature. Its [specification](specs/document-repository.json) is the canonical
behavior summary; the accepted
[adapter-capability decision](doc/adr/0001-portability-stops-at-explicit-adapter-capabilities.md)
explains why unsupported backend behavior fails instead of being emulated.

Do not infer a default-framework or cross-language parity promise from the
shared infrastructure fixture. That fixture pins one emitted protocol document;
it does not claim that another language exposes this repository API.
