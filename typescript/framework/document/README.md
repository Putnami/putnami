# @putnami/document

Portable document storage for Putnami with declarative collections, typed repositories, cursor pagination, and pluggable adapters.

Use `@putnami/database` when you need relational PostgreSQL data. Use `@putnami/document` when your model is document-shaped and you want the same Putnami repository ergonomics across memory and Firestore today, with room for more adapters later.

## Features

- Declarative `Collection()`, `Field()`, and `DocumentId()` builders
- Typed `Repository<T>` CRUD APIs
- Portable query operators and cursor-based pagination
- Named document stores via `document.<name>`
- Optional Firestore adapter loaded through dynamic import
- Runtime schema validation through `@putnami/runtime`
- Single-store transactions via `runInTransaction()`

## Installation

```bash
putnami deps add @putnami/document
```

Install the optional Firestore client only when you use that adapter:

```bash
putnami deps add @google-cloud/firestore
```

If you are working on this framework package inside the Putnami monorepo, install the peer on the package project:

```bash
putnami deps add @google-cloud/firestore --project /typescript/framework/document
```

## Quick Start

```ts
import { Collection, DocumentId, Email, Field, Optional, Repository } from '@putnami/document';

export const UsersCollection = Collection(
  'users',
  {
    id: DocumentId(String),
    email: Field(Email),
    name: Field(String),
    nickname: Field(Optional(String)),
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

Register the plugin so cached adapters are closed on shutdown:

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () => application().use(document());
```

Configure the default store:

```yaml
document:
  backend: memory
```

Or split stores by name:

```yaml
document:
  backend: memory

  audit:
    backend: firestore
    projectId: my-project
    emulatorHost: 127.0.0.1:8080
```

Use the repository:

```ts
const users = new UserRepository();

await users.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice',
});

const user = await users.get('user-123');

const page = await users.find(
  { email: { not: 'blocked@example.com' } },
  {
    limit: 20,
    orderBy: { field: 'name', direction: 'asc' },
  },
);
```

`save()` replaces the stored document by default. Use `merge: true` for patch-style writes:

```ts
await users.save(
  {
    id: 'user-123',
    name: 'Alice Cooper',
  },
  { merge: true },
);
```

## Query Model

The portable filter subset is intentionally small:

- equality: `email: 'alice@example.com'`
- comparison: `{ age: { gte: 18 } }`
- set membership: `{ status: { in: ['active', 'pending'] } }`
- array membership: `{ tags: { contains: 'vip' } }`
- field presence: `{ nickname: { exists: true } }`

Pagination is cursor-based:

```ts
const firstPage = await users.find(
  {},
  {
    limit: 10,
    orderBy: { field: 'name', direction: 'asc' },
  },
);

const secondPage = await users.find(
  {},
  {
    limit: 10,
    orderBy: { field: 'name', direction: 'asc' },
    cursor: firstPage.nextCursor,
  },
);
```

There is no offset-based pagination API.

## Save And Delete Semantics

- `save(document)` performs a replace write
- `save(document, { merge: true })` merges into the existing document
- `saveMany(documents)` performs replace writes, validates each document fully, and lets the adapter chunk batch sizes
- `deleteMany(filters)` requires filters for safety

## Transactions

Use `runInTransaction()` for atomic work inside a single named store:

```ts
import { runInTransaction } from '@putnami/document';

await runInTransaction(async () => {
  await users.save({ id: 'user-123', email: 'alice@example.com', name: 'Alice' });
  await users.save({ id: 'user-456', email: 'bob@example.com', name: 'Bob' });
});
```

Cross-store access inside a transaction throws `TransactionNotSupported`.

## Indexes And Strict Mode

Collections can declare portable index metadata:

```ts
Collection(
  'users',
  {
    id: DocumentId(String),
    email: Field(String),
    createdAt: Field(String),
  },
  {
    indexes: [
      { fields: ['email'] },
      { partition: ['email'], sort: ['createdAt'] },
    ],
  },
);
```

Enable `document.strictIndexes: true` to make the repository reject queries that are not covered by declared indexes.

## Adapters

| Backend | Config | Notes |
|---------|--------|-------|
| `memory` | `document.backend: memory` | Great for tests and lightweight local use. Supports transactions and composite document ids. |
| `firestore` | `document.backend: firestore` | Loaded through optional peer dependency. Supports transactions and cursor pagination. Requires a single scalar document id. |

Both current adapters accept `consistency: 'strong' | 'eventual'` on reads. The public API keeps this explicit so stricter future adapters can reject unsupported strong reads cleanly.

## Public API

The package exports:

- builders: `Collection`, `Field`, `DocumentId`
- types: `InferCollection`, `InferDocumentId`, `CollectionDefinition`, `IndexDefinition`
- repository APIs: `Repository`, `QueryFilters`, `FindOptions`, `FindResult`
- lifecycle: `useBackend`, `closeBackend`, `closeAllBackends`, `document`
- transactions: `runInTransaction`
- errors: `DocumentError`, `IndexMissing`, `TransactionNotSupported`, `StrongConsistencyUnsupported`
- observability helpers from `./src/observability`
- runtime schema primitives re-exported from `@putnami/runtime`

## Documentation

- [Getting started](./doc/01-getting-started.md)
- [Repository and queries](./doc/02-repository.md)
- [Transactions](./doc/03-transactions.md)
- [Adapters and config](./doc/04-adapters-and-config.md)

## Support and contract

`@putnami/document` is a public, documented, maintained package classified
`stable`. The [document-repository specification](specs/document-repository.json)
and its accepted
[adapter-capability ADR](doc/adr/0001-portability-stops-at-explicit-adapter-capabilities.md)
define the promise: repository operations keep one meaning across adapters that
advertise them, unsupported capabilities fail explicitly, cursor order is
deterministic, writes return stored state, and transactions never pretend to be
atomic across named stores.

The package makes no default-framework or cross-language parity claim. Before
v1.0.0, minor `0.x` releases may still contain documented breaking changes;
strict compatibility across every pre-1.0 minor is not promised.
