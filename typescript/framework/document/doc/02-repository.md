# Repository And Queries

The `Repository<T>` API keeps document access typed while staying portable across adapters.

## Basic repository

```ts
import { Collection, DocumentId, Field, Repository } from '@putnami/document';

const UsersCollection = Collection('users', {
  id: DocumentId(String),
  email: Field(String),
  name: Field(String),
  age: Field(Number),
});

class UserRepository extends Repository<typeof UsersCollection> {
  constructor() {
    super(UsersCollection);
  }
}
```

## CRUD methods

- `get(id, { consistency? })`
- `exists(id, { consistency? })`
- `findOne(filters, { orderBy?, consistency? })`
- `find(filters, { limit?, cursor?, orderBy?, consistency? })`
- `save(document, { merge?, strict? })`
- `saveMany(documents)`
- `delete(id)`
- `deleteMany(filters)`

## Query filters

Portable filter operators:

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

Examples:

```ts
await repo.find({ email: 'alice@example.com' });

await repo.find({ age: { gte: 18, lt: 65 } });

await repo.find({ email: { in: ['alice@example.com', 'bob@example.com'] } });

await repo.find({ tags: { contains: 'vip' } });

await repo.find({ nickname: { exists: false } });
```

The public API intentionally does not expose regex, full-text, geo, offset pagination, or backend-native query DSLs.

## Ordering

Use structured `orderBy` values instead of raw strings:

```ts
await repo.find(
  {},
  {
    orderBy: { field: 'age', direction: 'desc' },
  },
);

await repo.find(
  {},
  {
    orderBy: [
      { field: 'age', direction: 'desc' },
      { field: 'email', direction: 'asc' },
    ],
  },
);
```

Repositories automatically add document-id fields as stable tiebreakers when needed.

## Cursor pagination

Pagination is cursor-based:

```ts
const firstPage = await repo.find({}, { limit: 10, orderBy: { field: 'email', direction: 'asc' } });

if (firstPage.nextCursor) {
  const secondPage = await repo.find(
    {},
    {
      limit: 10,
      cursor: firstPage.nextCursor,
      orderBy: { field: 'email', direction: 'asc' },
    },
  );
}
```

Treat cursors as opaque values. Do not parse or modify them in application code.

`limit` is honored exactly: `limit: 0` returns zero rows (it is not treated as
"unlimited"). An absent, negative, or `NaN` limit falls back to the default page
size.

## Save semantics

Replace write:

```ts
await repo.save({
  id: 'user-123',
  email: 'alice@example.com',
  name: 'Alice',
});
```

Merge write:

```ts
await repo.save(
  {
    id: 'user-123',
    name: 'Alice Updated',
  },
  { merge: true },
);
```

Validation rules:

- replace writes default to `strict: true`
- merge writes default to `strict: false`
- `saveMany()` fully validates each document and uses replace semantics in v1

Return value:

- `save()` and `saveMany()` return the document as **stored** (re-read after the
  write), so server-applied write transforms surface consistently across every
  adapter — the return is not a copy of the request body.

## Delete safety

`deleteMany()` requires filters:

```ts
await repo.deleteMany({ age: { lt: 18 } });
```

Calling `deleteMany({})` throws `DocumentError` with `DELETE_WITHOUT_FILTERS`.

## Index declarations

Collections declare portable index coverage:

```ts
const UsersCollection = Collection(
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

If `document.strictIndexes` is enabled, queries that are not covered by declared index metadata throw `IndexMissing`.

## Consistency

Read methods accept:

```ts
await repo.get('user-123', { consistency: 'strong' });
await repo.find({}, { consistency: 'eventual' });
```

The API is explicit so future eventual-only adapters can reject unsupported strong reads with `StrongConsistencyUnsupported`.
