# Transactions

`@putnami/document` exposes transactions as a portable application contract rather than leaking adapter-specific transaction objects.

## Basic usage

```ts
import { runInTransaction } from '@putnami/document';

await runInTransaction(async () => {
  await users.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice' });
  await users.save({ id: 'user-2', email: 'bob@example.com', name: 'Bob' });
});
```

Inside the transaction body, repository reads and writes automatically use the active transaction when they target the same store.

## Read-your-writes

```ts
await runInTransaction(async () => {
  await users.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice' });
  const user = await users.get('user-1');
  console.log(user?.name); // Alice
});
```

## Rollback on error

```ts
await runInTransaction(async () => {
  await users.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice' });
  throw new Error('boom');
});
```

If the callback throws, the transaction is rolled back and the error is rethrown.

## Single-store scope

Transactions are scoped to one named document store.

```ts
await runInTransaction(async () => {
  await users.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice' });
  await audit.save({ id: 'audit-1', action: 'created-user' });
}, { storeName: 'default' });
```

If `audit` uses `db: 'audit'`, this throws `TransactionNotSupported` with a `CROSS_STORE_TRANSACTION` code.

## Options

```ts
await runInTransaction(
  async () => {
    await users.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice' });
  },
  {
    storeName: 'default',
    timeoutMs: 5_000,
  },
);
```

- `storeName` chooses which configured store owns the transaction
- `timeoutMs` marks long-running transactions as unsupported and fails the operation

## Adapter behavior

- `memory` snapshots the store, runs the callback, and commits the snapshot on success
- `firestore` delegates to `firestore.runTransaction()`

If an adapter cannot honor transactions, `runInTransaction()` throws `TransactionNotSupported`.
