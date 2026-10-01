import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { DocumentErrorCode } from '../src/errors';
import { createFirestoreAdapter, createServerTransformingFirestoreClient, FakeFirestoreClient } from './firestore.fake';

describe('FirestoreAdapter', () => {
  specTest(
    'batches saveMany re-reads into one getAll round-trip per chunk',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'bulk-save-re-reads-are-chunked',
    },
    async () => {
      const client = new FakeFirestoreClient();
      const { adapter } = createFirestoreAdapter(client);

      // Two full chunks + a partial one exercises the per-chunk batched re-read.
      const items = Array.from({ length: 1100 }, (_, index) => ({
        id: `user-${index}`,
        data: { id: `user-${index}`, age: index },
      }));

      const saved = await adapter.saveMany('users', items);

      // One batched read per 500-item chunk (500 + 500 + 100 => 3 chunks),
      // never a sequential per-document get().
      expect(client.getAllCalls).toBe(3);
      expect(client.singleGetCalls).toBe(0);

      // Contract + order preserved: getAll returns snapshots in ref order.
      expect(saved).toHaveLength(1100);
      expect(saved.map((doc) => doc['id'])).toEqual(items.map((item) => item.data.id));
    },
  );

  it('returns the stored (server-transformed) document from batched saveMany re-reads', async () => {
    const client = createServerTransformingFirestoreClient();
    const { adapter } = createFirestoreAdapter(client);

    const saved = await adapter.saveMany('users', [
      { id: 'a', data: { id: 'a', age: 1 } },
      { id: 'b', data: { id: 'b', age: 2 } },
    ]);

    // The re-read reflects the stored doc (serverStamp), not the request body.
    expect(client.getAllCalls).toBe(1);
    expect(saved).toEqual([
      { id: 'a', age: 1, serverStamp: true },
      { id: 'b', age: 2, serverStamp: true },
    ]);
  });

  it('produces nextCursor for ordinary queries', async () => {
    const { adapter } = createFirestoreAdapter();

    await adapter.saveMany('users', [
      { id: 'a', data: { id: 'a', age: 1 } },
      { id: 'b', data: { id: 'b', age: 2 } },
      { id: 'c', data: { id: 'c', age: 3 } },
    ]);

    const firstPage = await adapter.find('users', [], {
      limit: 2,
      orderBy: [{ field: 'age', direction: 'asc' }],
      consistency: 'strong',
    });

    expect(firstPage.items.map((item) => item['id'])).toEqual(['a', 'b']);
    expect(firstPage.nextCursor).toBeDefined();

    const secondPage = await adapter.find('users', [], {
      limit: 2,
      cursor: firstPage.nextCursor,
      orderBy: [{ field: 'age', direction: 'asc' }],
      consistency: 'strong',
    });

    expect(secondPage.items.map((item) => item['id'])).toEqual(['c']);
  });

  specTest(
    'chunks exists:false scans instead of issuing an unbounded query',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'an-exists-false-scan-is-chunked',
    },
    async () => {
      const { adapter, client } = createFirestoreAdapter();

      const items = Array.from({ length: 120 }, (_, index) => ({
        id: `user-${index.toString().padStart(3, '0')}`,
        data:
          index === 75 || index === 110
            ? { id: `user-${index.toString().padStart(3, '0')}`, age: index }
            : { id: `user-${index.toString().padStart(3, '0')}`, age: index, nickname: `nick-${index}` },
      }));
      await adapter.saveMany('users', items);

      const firstPage = await adapter.find('users', [{ field: 'nickname', op: 'exists', value: false }], {
        limit: 1,
        orderBy: [{ field: 'age', direction: 'asc' }],
        consistency: 'strong',
      });

      expect(firstPage.items.map((item) => item['id'])).toEqual(['user-075']);
      expect(firstPage.nextCursor).toBeDefined();
      expect(client.queryLimits.length).toBeGreaterThan(1);
      expect(client.queryLimits.every((limit) => typeof limit === 'number' && limit > 0)).toBe(true);

      const secondPage = await adapter.find('users', [{ field: 'nickname', op: 'exists', value: false }], {
        limit: 1,
        cursor: firstPage.nextCursor,
        orderBy: [{ field: 'age', direction: 'asc' }],
        consistency: 'strong',
      });

      expect(secondPage.items.map((item) => item['id'])).toEqual(['user-110']);
    },
  );

  specTest(
    'deletes all matching documents beyond a single batch',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'delete-many-spans-more-than-one-batch',
    },
    async () => {
      const { adapter } = createFirestoreAdapter();

      await adapter.saveMany(
        'users',
        Array.from({ length: 1200 }, (_, index) => ({
          id: `user-${index}`,
          data: { id: `user-${index}`, tenant: 'acme' },
        })),
      );

      expect(await adapter.deleteMany('users', [{ field: 'tenant', op: 'eq', value: 'acme' }])).toBe(1200);

      const remaining = await adapter.find('users', [], {
        limit: 1,
        orderBy: [{ field: 'id', direction: 'asc' }],
        consistency: 'strong',
      });

      expect(remaining.items).toEqual([]);
    },
  );

  specTest(
    'throws a clear error for incompatible negative filters',
    {
      feature: 'typescript/document-repository',
      requirement: 'adapter-capabilities',
      check: 'the-firestore-adapter-reports-a-clear-negative-filter-error',
    },
    async () => {
      const { adapter } = createFirestoreAdapter();

      await expect(
        adapter.find(
          'users',
          [
            { field: 'nickname', op: 'exists', value: true },
            { field: 'email', op: 'ne', value: 'blocked@example.com' },
          ],
          {
            limit: 10,
            orderBy: [{ field: 'email', direction: 'asc' }],
            consistency: 'strong',
          },
        ),
      ).rejects.toMatchObject({
        code: DocumentErrorCode.QueryNotSupported,
      });
    },
  );
});
