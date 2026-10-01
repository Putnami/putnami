# Document Storage

Use `@putnami/document` when your data is document-shaped and you want typed repositories, schema validation, cursor pagination, and pluggable document backends.

This sits next to:

- `@putnami/database` for relational PostgreSQL data
- `@putnami/storage` for blobs and file uploads

## Installation

```bash
putnami deps add @putnami/document
```

If you use Firestore, install its optional peer dependency too:

```bash
putnami deps add @google-cloud/firestore
```

## Enable the plugin

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () => application().use(document());
```

The plugin closes cached adapters on shutdown.

## Define collections

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

`Collection()` declares:

- the collection name
- the document schema
- optional named-store and index metadata

`DocumentId()` marks one or more fields as the document id. The public type system supports composite ids. The in-memory adapter supports them today; the Firestore adapter currently requires a single scalar id.

## Create a repository

```ts
import { Repository } from '@putnami/document';
import { UsersCollection } from './users.collection';

export class UserRepository extends Repository<typeof UsersCollection> {
  constructor() {
    super(UsersCollection);
  }
}
```

## Configuration

Default store:

```yaml
document:
  backend: memory
```

Named stores:

```yaml
document:
  backend: memory

  audit:
    backend: firestore
    projectId: my-project
    emulatorHost: 127.0.0.1:8080
```

Bind a collection to a named store:

```ts
Collection(
  'audit',
  {
    id: DocumentId(String),
    action: Field(String),
  },
  { db: 'audit' },
);
```

## Repository API

```ts
const users = new UserRepository();

await users.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice',
  createdAt: new Date().toISOString(),
});

const user = await users.get('user-123');
const exists = await users.exists('user-123');

const result = await users.find(
  {
    email: { not: 'blocked@example.com' },
    createdAt: { exists: true },
  },
  {
    limit: 20,
    orderBy: { field: 'createdAt', direction: 'desc' },
  },
);
```

Available methods:

- `get(id, { consistency? })`
- `exists(id, { consistency? })`
- `findOne(filters, options?)`
- `find(filters, { limit?, cursor?, orderBy?, consistency? })`
- `save(document, { merge?, strict? })`
- `saveMany(documents)`
- `delete(id)`
- `deleteMany(filters)`

## Query model

Portable operators:

- equality: `equals`, `not`
- comparison: `gt`, `gte`, `lt`, `lte`
- set membership: `in`, `notIn`
- array membership: `contains`
- presence: `exists`

Ordering is structured, not SQL text:

```ts
await users.find({}, { orderBy: { field: 'email', direction: 'asc' } });
```

Pagination is cursor-based:

```ts
const firstPage = await users.find({}, { limit: 10, orderBy: { field: 'email', direction: 'asc' } });

const secondPage = await users.find({}, {
  limit: 10,
  cursor: firstPage.nextCursor,
  orderBy: { field: 'email', direction: 'asc' },
});
```

There is no offset API.

## Save semantics

`save()` is replace-by-default:

```ts
await users.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice Updated',
});
```

Use `merge: true` for patch-style writes:

```ts
await users.save(
  {
    id: 'user-123',
    nickname: 'ally',
  },
  { merge: true },
);
```

Both calls return the stored document read back from the adapter, not the input
object. That keeps server-applied transforms visible and prevents a caller from
mistaking its mutable request value for stored state. `saveMany()` follows the
same read-back contract and always performs full-document validation.

Schema validation uses the same primitives exported by `@putnami/runtime`.

## Transactions

```ts
import { runInTransaction } from '@putnami/document';

await runInTransaction(async () => {
  await users.save({ id: 'user-123', email: 'alice@example.com', name: 'Alice' });
  await users.save({ id: 'user-456', email: 'bob@example.com', name: 'Bob' });
});
```

Transactions are scoped to a single named store. Cross-store repository access inside a transaction throws `TransactionNotSupported`.

## Index declarations

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

Enable strict index enforcement in config:

```yaml
document:
  strictIndexes: true
```

Queries that are not covered by declared index metadata throw `IndexMissing`.

## Backends

### Memory

Good for tests and lightweight local use.

- no external dependency
- transactions supported
- composite ids supported

### Firestore

Good for managed remote document storage.

- loaded through optional peer dependency
- transactions supported
- single scalar document id only
- supports `projectId`, `databaseId`, `emulatorHost`, and `credentials`

Inline credentials and key-file paths are configuration inputs only. Parse
errors identify the bad setting without echoing its secret value, and generated
infrastructure requirements contain store and database names, never credentials.

## Lifecycle And Observability

Use `useBackend()`, `closeBackend()`, and `closeAllBackends()` when you need manual backend control. Repositories also emit per-collection metrics and structured logs for `get`, `find`, `save`, `saveMany`, `delete`, and `deleteMany`.

## Support and compatibility

`@putnami/document` is a public, documented, maintained package classified
`stable`. Its document-repository specification and accepted adapter-capability
ADR live next to the package source. The supported contract is the structured
repository vocabulary described here: adapters either implement an operation
with the same observable meaning or reject an unsupported capability
explicitly; transactions never span named stores; pagination stays cursor-based
and deterministic; and credential values never enter generated manifests or
errors.

The package does not claim to be the default persistence framework and makes no
cross-language parity promise. Before v1.0.0, minor `0.x` releases may contain
documented breaking changes.

## See also

- [Persistence](/docs/frameworks/typescript/persistence)
- [Storage](/docs/frameworks/typescript/storage)
