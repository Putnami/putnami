import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { Int, resetConfigLoader, runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { TransactionNotSupported } from '../src/errors';
import { closeAllBackends } from '../src/factory';
import { Repository } from '../src/repository/repository';
import { runInTransaction } from '../src/transaction';

describe('document transactions', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
    resetConfigLoader();
    process.env.CONFIG_DATA = JSON.stringify({
      document: {
        backend: 'memory',
        audit: {
          backend: 'memory',
        },
      },
    });
  });

  afterEach(async () => {
    await closeAllBackends();
    setCollector(undefined);
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  function usersRepo() {
    return new Repository(
      Collection(`users_${crypto.randomUUID()}`, {
        id: DocumentId(String),
        balance: Field(Int),
      }),
    );
  }

  function auditRepo() {
    return new Repository(
      Collection(
        `audit_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          amount: Field(Int),
        },
        { db: 'audit' },
      ),
    );
  }

  specTest(
    'commits writes atomically and supports read-your-writes',
    {
      feature: 'typescript/document-repository',
      requirement: 'transaction-atomicity',
      check: 'a-transaction-commits-atomically-with-read-your-writes',
    },
    async () => {
      const repo = usersRepo();

      await runInContext({}, async () => {
        await runInTransaction(async () => {
          await repo.save({ id: 'user-1', balance: 10 });
          expect(await repo.get('user-1')).toEqual({ id: 'user-1', balance: 10 });
        });
      });

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.transaction.started']).toBe(1);
      expect(bucket.counters['document.transaction.committed']).toBe(1);
      expect(bucket.histograms['document.transaction.duration']).toBeDefined();
      expect(bucket.gauges['document.transaction.active']).toBe(0);

      expect(await repo.get('user-1')).toEqual({ id: 'user-1', balance: 10 });
    },
  );

  specTest(
    'rolls back when the transaction body throws',
    {
      feature: 'typescript/document-repository',
      requirement: 'transaction-atomicity',
      check: 'a-throwing-body-rolls-the-transaction-back',
    },
    async () => {
      const repo = usersRepo();

      try {
        await runInContext({}, async () => {
          await runInTransaction(async () => {
            await repo.save({ id: 'user-1', balance: 10 });
            throw new Error('boom');
          });
        });
        expect.unreachable();
      } catch (error) {
        expect((error as Error).message).toBe('boom');
      }

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.transaction.started']).toBe(1);
      expect(bucket.counters['document.transaction.rolled_back']).toBe(1);
      expect(bucket.counters['document.transaction.committed']).toBeUndefined();
      expect(bucket.histograms['document.transaction.duration']).toBeDefined();
      expect(bucket.gauges['document.transaction.active']).toBe(0);

      expect(await repo.get('user-1')).toBeUndefined();
    },
  );

  specTest(
    'rejects cross-store access inside a transaction',
    {
      feature: 'typescript/document-repository',
      requirement: 'transaction-atomicity',
      check: 'a-second-store-is-refused-inside-a-transaction',
    },
    async () => {
      const repo = usersRepo();
      const audit = auditRepo();

      try {
        await runInContext({}, async () => {
          await runInTransaction(async () => {
            await repo.save({ id: 'user-1', balance: 10 });
            await audit.save({ id: 'audit-1', amount: 10 });
          });
        });
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(TransactionNotSupported);
      }

      expect(await repo.get('user-1')).toBeUndefined();
    },
  );

  specTest(
    'deletes every matching document inside a transaction across multiple pages',
    {
      feature: 'typescript/document-repository',
      requirement: 'transaction-atomicity',
      check: 'a-transactional-delete-spans-multiple-pages',
    },
    async () => {
      const repo = usersRepo();

      await repo.saveMany(
        Array.from({ length: 1200 }, (_, index) => ({
          id: `user-${index}`,
          balance: 10,
        })),
      );

      await runInContext({}, async () => {
        await runInTransaction(async () => {
          expect(await repo.deleteMany({ balance: 10 })).toBe(1200);
        });
      });

      expect((await repo.find()).items).toEqual([]);
    },
  );
});
