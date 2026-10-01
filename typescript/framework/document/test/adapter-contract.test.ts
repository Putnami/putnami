import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { DocumentAdapter } from '../src/adapter/document.adapter';
import { MemoryAdapter } from '../src/adapter/memory.adapter';
import { DocumentErrorCode } from '../src/errors';
import { createFirestoreAdapter, createServerTransformingFirestoreClient } from './firestore.fake';

interface ContractOptions {
  /** Whether the backend accepts composite (object) document ids. */
  supportsCompositeIds: boolean;
  /**
   * Whether the backend rejects multiple negative filters in one query
   * (Firestore does; memory does not).
   */
  rejectsMultipleNegativeFilters: boolean;
  /**
   * Builds an adapter whose backing store applies a known server-side write
   * transform (stamping `serverStamp: true`) when provided. Used to prove the
   * save contract returns the *stored* document, not the request body. Omit for
   * backends that apply no transform (memory).
   */
  serverTransformingAdapter?: () => DocumentAdapter;
}

function adapterContract(name: string, createAdapter: () => DocumentAdapter, options: ContractOptions) {
  describe(name, () => {
    let adapter: DocumentAdapter;

    afterEach(async () => {
      await adapter?.close();
    });

    specTest(
      'supports save/get/exists/delete',
      {
        feature: 'typescript/document-repository',
        requirement: 'adapter-capabilities',
        check: 'the-shared-contract-covers-save-get-exists-delete',
      },
      async () => {
        adapter = createAdapter();

        await adapter.save('users', 'user-1', { id: 'user-1', name: 'Alice' });

        expect(await adapter.exists('users', 'user-1', 'strong')).toBe(true);
        expect(await adapter.get('users', 'user-1', 'strong')).toEqual({ id: 'user-1', name: 'Alice' });
        expect(await adapter.delete('users', 'user-1')).toEqual({ id: 'user-1', name: 'Alice' });
        expect(await adapter.get('users', 'user-1', 'strong')).toBeUndefined();
      },
    );

    specTest(
      'supports filter semantics and cursor round-trips',
      {
        feature: 'typescript/document-repository',
        requirement: 'portable-query',
        check: 'filters-and-cursors-round-trip-at-the-adapter',
      },
      async () => {
        adapter = createAdapter();

        await adapter.saveMany('users', [
          { id: 'a', data: { id: 'a', age: 1, tags: ['vip'] } },
          { id: 'b', data: { id: 'b', age: 2, tags: ['team'] } },
          { id: 'c', data: { id: 'c', age: 3, tags: ['vip'] } },
        ]);

        const firstPage = await adapter.find('users', [{ field: 'tags', op: 'contains', value: 'vip' }], {
          limit: 1,
          orderBy: [{ field: 'age', direction: 'asc' }],
          consistency: 'strong',
        });

        expect(firstPage.items).toEqual([{ id: 'a', age: 1, tags: ['vip'] }]);
        expect(firstPage.nextCursor).toBeDefined();

        const secondPage = await adapter.find('users', [{ field: 'tags', op: 'contains', value: 'vip' }], {
          limit: 1,
          cursor: firstPage.nextCursor,
          orderBy: [{ field: 'age', direction: 'asc' }],
          consistency: 'strong',
        });

        expect(secondPage.items).toEqual([{ id: 'c', age: 3, tags: ['vip'] }]);
      },
    );

    specTest(
      'honors limit:0 as zero rows rather than an unbounded scan',
      {
        feature: 'typescript/document-repository',
        requirement: 'bounded-bulk-behavior',
        check: 'a-zero-limit-means-zero-rows-at-the-adapter-too',
      },
      async () => {
        adapter = createAdapter();

        await adapter.saveMany('users', [
          { id: 'a', data: { id: 'a', age: 1 } },
          { id: 'b', data: { id: 'b', age: 2 } },
          { id: 'c', data: { id: 'c', age: 3 } },
        ]);

        const page = await adapter.find('users', [], {
          limit: 0,
          orderBy: [{ field: 'age', direction: 'asc' }],
          consistency: 'strong',
        });

        expect(page.items).toEqual([]);
      },
    );

    specTest(
      'save returns the stored document, not a live reference to the input',
      {
        feature: 'typescript/document-repository',
        requirement: 'stored-write-result',
        check: 'save-returns-the-stored-document-not-the-input-reference',
      },
      async () => {
        adapter = createAdapter();

        const input: Record<string, unknown> = { id: 'user-1', name: 'Alice', tags: ['vip'] };
        const saved = await adapter.save('users', 'user-1', input);

        // Mutating the input after the write must not change the returned document
        // nor the stored document: the return is a decoupled snapshot of storage.
        (input as { name: string }).name = 'Mutated';
        (input['tags'] as string[]).push('mutated');

        expect((saved as { name: string }).name).toBe('Alice');
        expect((saved as { tags: string[] }).tags).toEqual(['vip']);
        expect(await adapter.get('users', 'user-1', 'strong')).toEqual({ id: 'user-1', name: 'Alice', tags: ['vip'] });
      },
    );

    specTest(
      'saveMany returns stored documents decoupled from the inputs',
      {
        feature: 'typescript/document-repository',
        requirement: 'stored-write-result',
        check: 'save-many-returns-documents-decoupled-from-the-inputs',
      },
      async () => {
        adapter = createAdapter();

        const items = [
          { id: 'a', data: { id: 'a', name: 'Alice' } as Record<string, unknown> },
          { id: 'b', data: { id: 'b', name: 'Bob' } as Record<string, unknown> },
        ];
        const saved = await adapter.saveMany('users', items);

        (items[0]?.data as { name: string }).name = 'Mutated';

        expect(saved.map((doc) => (doc as { name: string }).name)).toEqual(['Alice', 'Bob']);
        expect(await adapter.get('users', 'a', 'strong')).toEqual({ id: 'a', name: 'Alice' });
      },
    );

    specTest(
      'commits and rolls back transactions atomically',
      {
        feature: 'typescript/document-repository',
        requirement: 'transaction-atomicity',
        check: 'commit-and-rollback-are-atomic-at-the-adapter',
      },
      async () => {
        adapter = createAdapter();

        await adapter.runInTransaction(async (tx) => {
          await tx.save('users', 'user-1', { id: 'user-1', name: 'Alice' });
          expect(await tx.get('users', 'user-1')).toEqual({ id: 'user-1', name: 'Alice' });
        });

        expect(await adapter.get('users', 'user-1', 'strong')).toEqual({ id: 'user-1', name: 'Alice' });

        try {
          await adapter.runInTransaction(async (tx) => {
            await tx.save('users', 'user-2', { id: 'user-2', name: 'Bob' });
            throw new Error('rollback');
          });
          expect.unreachable();
        } catch (error) {
          expect((error as Error).message).toBe('rollback');
        }

        expect(await adapter.get('users', 'user-2', 'strong')).toBeUndefined();
      },
    );

    if (options.supportsCompositeIds) {
      specTest(
        'round-trips composite document ids',
        {
          feature: 'typescript/document-repository',
          requirement: 'adapter-capabilities',
          check: 'a-composite-document-id-round-trips-where-supported',
        },
        async () => {
          adapter = createAdapter();

          const id = { tenant: 'acme', user: 'user-1' };
          await adapter.save('memberships', id, { tenant: 'acme', user: 'user-1', role: 'admin' });

          expect(await adapter.exists('memberships', id, 'strong')).toBe(true);
          expect(await adapter.get('memberships', id, 'strong')).toEqual({
            tenant: 'acme',
            user: 'user-1',
            role: 'admin',
          });
        },
      );
    } else {
      specTest(
        'rejects composite document ids with CompositeIdNotSupported',
        {
          feature: 'typescript/document-repository',
          requirement: 'adapter-capabilities',
          check: 'a-composite-document-id-is-refused-where-unsupported',
        },
        async () => {
          adapter = createAdapter();

          await expect(
            adapter.save('memberships', { tenant: 'acme', user: 'user-1' }, { role: 'admin' }),
          ).rejects.toMatchObject({ code: DocumentErrorCode.CompositeIdNotSupported });
        },
      );
    }

    if (options.rejectsMultipleNegativeFilters) {
      specTest(
        'rejects multiple negative filters in a single query',
        {
          feature: 'typescript/document-repository',
          requirement: 'adapter-capabilities',
          check: 'incompatible-negative-filters-are-refused',
        },
        async () => {
          adapter = createAdapter();

          await expect(
            adapter.find(
              'users',
              [
                { field: 'name', op: 'ne', value: 'blocked' },
                { field: 'email', op: 'ne', value: 'blocked@example.com' },
              ],
              { limit: 10, orderBy: [{ field: 'email', direction: 'asc' }], consistency: 'strong' },
            ),
          ).rejects.toMatchObject({ code: DocumentErrorCode.QueryNotSupported });
        },
      );
    } else {
      specTest(
        'allows multiple negative filters in a single query',
        {
          feature: 'typescript/document-repository',
          requirement: 'adapter-capabilities',
          check: 'multiple-negative-filters-are-allowed-where-supported',
        },
        async () => {
          adapter = createAdapter();

          await adapter.saveMany('users', [
            { id: 'a', data: { id: 'a', name: 'Alice', email: 'alice@example.com' } },
            { id: 'b', data: { id: 'b', name: 'blocked', email: 'blocked@example.com' } },
          ]);

          const result = await adapter.find(
            'users',
            [
              { field: 'name', op: 'ne', value: 'blocked' },
              { field: 'email', op: 'ne', value: 'blocked@example.com' },
            ],
            { limit: 10, orderBy: [{ field: 'name', direction: 'asc' }], consistency: 'strong' },
          );

          expect(result.items.map((item) => item['id'])).toEqual(['a']);
        },
      );
    }

    if (options.serverTransformingAdapter) {
      const buildTransforming = options.serverTransformingAdapter;

      specTest(
        'save reflects a server-applied write transform (stored doc, not echo)',
        {
          feature: 'typescript/document-repository',
          requirement: 'stored-write-result',
          check: 'save-reflects-a-server-applied-write-transform',
        },
        async () => {
          adapter = buildTransforming();

          const saved = await adapter.save('users', 'user-1', { id: 'user-1', name: 'Alice' });

          // The backing store stamps `serverStamp: true` on write. A return that
          // echoed the request body would lack it; a re-read of storage includes
          // it. This is what makes the echo-vs-stored divergence visible.
          expect(saved).toEqual({ id: 'user-1', name: 'Alice', serverStamp: true });
          expect(await adapter.get('users', 'user-1', 'strong')).toEqual({
            id: 'user-1',
            name: 'Alice',
            serverStamp: true,
          });
        },
      );

      specTest(
        'saveMany reflects a server-applied write transform',
        {
          feature: 'typescript/document-repository',
          requirement: 'stored-write-result',
          check: 'save-many-reflects-a-server-applied-write-transform',
        },
        async () => {
          adapter = buildTransforming();

          const saved = await adapter.saveMany('users', [
            { id: 'a', data: { id: 'a', name: 'Alice' } },
            { id: 'b', data: { id: 'b', name: 'Bob' } },
          ]);

          expect(saved).toEqual([
            { id: 'a', name: 'Alice', serverStamp: true },
            { id: 'b', name: 'Bob', serverStamp: true },
          ]);
        },
      );
    }
  });
}

adapterContract('MemoryAdapter contract', () => new MemoryAdapter(), {
  supportsCompositeIds: true,
  rejectsMultipleNegativeFilters: false,
  // Memory applies no server-side write transform; its snapshot/decoupling
  // behavior is covered by the shared save/saveMany cases above.
});

adapterContract('FirestoreAdapter contract', () => createFirestoreAdapter().adapter, {
  supportsCompositeIds: false,
  rejectsMultipleNegativeFilters: true,
  serverTransformingAdapter: () => createFirestoreAdapter(createServerTransformingFirestoreClient()).adapter,
});
