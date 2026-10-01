import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import {
  Column,
  Key,
  Migrator,
  Repository,
  Table,
  commit,
  rollback,
  runInTransaction,
  useTxConnection,
  withTransaction,
} from '../src/index';
import { cleanupTransaction } from '../src/transaction';
import { database } from '../src/factory';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';
import { setPrimaryDatasource } from '../src/primary-datasource';

const DB_NAME = 'tx-test';
const dockerAvailable = await isDockerAvailable();

const TxTestTable = Table(
  'tx_test_items',
  {
    id: Key(String),
    name: Column(String),
    value: Column(Number),
  },
  { db: DB_NAME },
);

const PrimaryTxTestTable = Table('tx_test_items', {
  id: Key(String),
  name: Column(String),
  value: Column(Number),
});

class TxTestRepository extends Repository<typeof TxTestTable> {
  constructor() {
    super(TxTestTable);
  }
}

class PrimaryTxTestRepository extends Repository<typeof PrimaryTxTestTable> {
  constructor() {
    super(PrimaryTxTestTable);
  }
}

let repository: TxTestRepository;
let sql: Awaited<ReturnType<typeof database>>;

describe.skipIf(!dockerAvailable)('Transaction support', () => {
  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);

    repository = new TxTestRepository();
    sql = await database(DB_NAME);

    // A prior test run may have dropped the table while leaving its
    // migration row behind (afterAll only drops the table). Clear both
    // so the Migrator below actually re-creates the table.
    await sql`CREATE SCHEMA IF NOT EXISTS migration`;
    await sql`CREATE TABLE IF NOT EXISTS migration.migrations (
      id TEXT PRIMARY KEY, db_name TEXT NOT NULL, name TEXT NOT NULL,
      hash TEXT NOT NULL, executed_at TEXT NOT NULL, execution_time_ms INTEGER NOT NULL,
      success INTEGER NOT NULL, error_message TEXT, down_sql TEXT, down_hash TEXT
    )`;
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DB_NAME}`;

    const migrator = new Migrator(DB_NAME, [
      {
        name: 'tx-test/create-tx-test-table',
        sql: `CREATE TABLE IF NOT EXISTS tx_test_items (
          id TEXT PRIMARY KEY,
          name TEXT NOT NULL,
          value NUMERIC NOT NULL
        );`,
      },
    ]);
    await migrator.up();

    await sql`DELETE FROM tx_test_items`;
  }, 30_000);

  afterEach(async () => {
    setPrimaryDatasource(undefined);
    await sql`DELETE FROM tx_test_items`;
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await sql`DROP TABLE IF EXISTS tx_test_items`;
  });

  describe('without transaction', () => {
    it('should work as before — writes are immediate', async () => {
      await runInContext({}, async () => {
        await repository.save({ id: '1', name: 'item-1', value: 100 });
      });

      // Verify outside context
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = '1'`;
      expect(rows.length).toBe(1);
      expect(rows[0].name).toBe('item-1');
    });

    it('should read normally', async () => {
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('read-1', 'read-item', 42)`;

      await runInContext({}, async () => {
        const item = await repository.findOne({ id: 'read-1' });
        expect(item).toBeDefined();
        expect(item?.name).toBe('read-item');
      });
    });

    it('should let raw transaction connections inherit the primary datasource', async () => {
      setPrimaryDatasource({ name: DB_NAME });
      const primaryRepository = new PrimaryTxTestRepository();

      await runInContext({}, async () => {
        withTransaction();

        await primaryRepository.save({ id: 'primary-raw-1', name: 'primary-raw', value: 7 });
        const raw = await useTxConnection(undefined, 'read');
        const rows = await raw`SELECT * FROM tx_test_items WHERE id = 'primary-raw-1'`;
        expect(rows.length).toBe(1);

        await rollback();
      });

      const outsideRows = await sql`SELECT * FROM tx_test_items WHERE id = 'primary-raw-1'`;
      expect(outsideRows.length).toBe(0);
    });
  });

  describe('withTransaction + commit', () => {
    it('should commit writes atomically', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'tx-1', name: 'tx-item-1', value: 10 });
        await repository.save({ id: 'tx-2', name: 'tx-item-2', value: 20 });

        // Before commit — not visible outside the transaction
        const outsideRows = await sql`SELECT * FROM tx_test_items WHERE id IN ('tx-1', 'tx-2')`;
        expect(outsideRows.length).toBe(0);

        await commit();
      });

      // After commit — visible
      const rows = await sql`SELECT * FROM tx_test_items WHERE id IN ('tx-1', 'tx-2') ORDER BY id`;
      expect(rows.length).toBe(2);
      expect(rows[0].name).toBe('tx-item-1');
      expect(rows[1].name).toBe('tx-item-2');
    });

    it('should support read-your-writes within a transaction', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'ryw-1', name: 'read-your-write', value: 99 });

        // Read within the same transaction should see the write
        const item = await repository.findOne({ id: 'ryw-1' });
        expect(item).toBeDefined();
        expect(item?.name).toBe('read-your-write');

        await commit();
      });
    });

    it('should be a no-op commit when only reads happened', async () => {
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('readonly-1', 'readonly', 0)`;

      await runInContext({}, async () => {
        withTransaction();

        const item = await repository.findOne({ id: 'readonly-1' });
        expect(item).toBeDefined();

        // Commit should be a no-op — no tx was ever opened
        await commit();
      });
    });
  });

  describe('withTransaction + rollback', () => {
    it('should rollback writes', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'rb-1', name: 'will-rollback', value: 50 });
        await repository.save({ id: 'rb-2', name: 'will-rollback-too', value: 60 });

        await rollback();
      });

      // After rollback — nothing persisted
      const rows = await sql`SELECT * FROM tx_test_items WHERE id IN ('rb-1', 'rb-2')`;
      expect(rows.length).toBe(0);
    });

    it('should rollback on error and rethrow', async () => {
      let threw = false;

      try {
        await runInContext({}, async () => {
          withTransaction();

          try {
            await repository.save({ id: 'err-1', name: 'before-error', value: 1 });
            throw new Error('Something went wrong');
          } finally {
            await cleanupTransaction();
          }
        });
      } catch (e) {
        threw = true;
        expect((e as Error).message).toBe('Something went wrong');
      }

      expect(threw).toBe(true);

      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'err-1'`;
      expect(rows.length).toBe(0);
    });
  });

  describe('cleanup safety net', () => {
    it('should auto-rollback uncommitted transaction', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'cleanup-1', name: 'uncommitted', value: 77 });

        // Forgot to commit or rollback — cleanupTransaction to the rescue
        const didRollback = await cleanupTransaction();
        expect(didRollback).toBe(true);
      });

      // Data should not be persisted
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'cleanup-1'`;
      expect(rows.length).toBe(0);
    });

    it('should return false when no tx was active', async () => {
      await runInContext({}, async () => {
        const didRollback = await cleanupTransaction();
        expect(didRollback).toBe(false);
      });
    });

    it('should return false when tx was read-only', async () => {
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('cleanup-ro', 'readonly', 0)`;

      await runInContext({}, async () => {
        withTransaction();

        await repository.findOne({ id: 'cleanup-ro' });

        const didRollback = await cleanupTransaction();
        expect(didRollback).toBe(false);
      });
    });
  });

  describe('nested withTransaction', () => {
    it('should join the outer transaction (no-op)', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'nested-1', name: 'outer', value: 1 });

        // Nested withTransaction — should not open a new tx
        withTransaction();

        await repository.save({ id: 'nested-2', name: 'inner', value: 2 });

        // Single commit covers both
        await commit();
      });

      const rows = await sql`SELECT * FROM tx_test_items WHERE id IN ('nested-1', 'nested-2') ORDER BY id`;
      expect(rows.length).toBe(2);
    });
  });

  describe('runInTransaction (recommended API)', () => {
    it('should auto-commit on success', async () => {
      await runInContext({}, async () => {
        const result = await runInTransaction(async () => {
          await repository.save({ id: 'rit-1', name: 'auto-commit-1', value: 10 });
          await repository.save({ id: 'rit-2', name: 'auto-commit-2', value: 20 });
          return 'done';
        });

        expect(result).toBe('done');
      });

      // After auto-commit — visible
      const rows = await sql`SELECT * FROM tx_test_items WHERE id IN ('rit-1', 'rit-2') ORDER BY id`;
      expect(rows.length).toBe(2);
      expect(rows[0].name).toBe('auto-commit-1');
      expect(rows[1].name).toBe('auto-commit-2');
    });

    it('should auto-rollback on error and rethrow', async () => {
      let threw = false;

      try {
        await runInContext({}, async () => {
          await runInTransaction(async () => {
            await repository.save({ id: 'rit-err-1', name: 'will-rollback', value: 99 });
            throw new Error('boom');
          });
        });
      } catch (e) {
        threw = true;
        expect((e as Error).message).toBe('boom');
      }

      expect(threw).toBe(true);

      // Auto-rollback — nothing persisted
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'rit-err-1'`;
      expect(rows.length).toBe(0);
    });

    it('should return the callback result', async () => {
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('rit-ret-1', 'return-test', 42)`;

      const result = await runInContext({}, async () =>
        runInTransaction(async () => {
          const item = await repository.findOne({ id: 'rit-ret-1' });
          return item;
        }),
      );

      expect(result).toBeDefined();
      expect(result?.name).toBe('return-test');
    });

    it('should be a no-op when only reads happened', async () => {
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('rit-ro-1', 'read-only', 0)`;

      await runInContext({}, async () => {
        await runInTransaction(async () => {
          const item = await repository.findOne({ id: 'rit-ro-1' });
          expect(item).toBeDefined();
        });
      });
    });

    it('should not commit when nested inside an existing transaction', async () => {
      await runInContext({}, async () => {
        withTransaction();

        await repository.save({ id: 'rit-nested-outer', name: 'outer', value: 1 });

        await runInTransaction(async () => {
          await repository.save({ id: 'rit-nested-inner', name: 'inner', value: 2 });
        });

        // Inner runInTransaction should not commit the outer transaction.
        const outsideRows = await sql`
          SELECT * FROM tx_test_items WHERE id IN ('rit-nested-outer', 'rit-nested-inner')
        `;
        expect(outsideRows.length).toBe(0);

        await commit();
      });

      const rows = await sql`
        SELECT * FROM tx_test_items WHERE id IN ('rit-nested-outer', 'rit-nested-inner') ORDER BY id
      `;
      expect(rows.length).toBe(2);
    });

    it('should throw when called outside runInContext', async () => {
      let called = false;
      let error: Error | undefined;

      try {
        await runInTransaction(async () => {
          called = true;
          return 'ok';
        });
      } catch (e) {
        error = e as Error;
      }

      expect(called).toBe(false);
      expect(error).toBeDefined();
      expect(error?.message).toContain('runInContext');
    });

    it('should support concurrent writes in a single transaction', async () => {
      await runInContext({}, async () => {
        await runInTransaction(async () => {
          await Promise.all([
            repository.save({ id: 'rit-conc-1', name: 'concurrent-1', value: 1 }),
            repository.save({ id: 'rit-conc-2', name: 'concurrent-2', value: 2 }),
            repository.save({ id: 'rit-conc-3', name: 'concurrent-3', value: 3 }),
            repository.save({ id: 'rit-conc-4', name: 'concurrent-4', value: 4 }),
          ]);
        });
      });

      const rows = await sql`
        SELECT * FROM tx_test_items WHERE id IN ('rit-conc-1', 'rit-conc-2', 'rit-conc-3', 'rit-conc-4')
      `;
      expect(rows.length).toBe(4);
    });
  });

  describe('transaction timeout', () => {
    it('should auto-rollback after timeout', async () => {
      await runInContext({}, async () => {
        await runInTransaction(
          async () => {
            await repository.save({ id: 'timeout-1', name: 'will-timeout', value: 42 });

            // Wait longer than the timeout
            await new Promise((resolve) => setTimeout(resolve, 150));
          },
          { timeoutMs: 50 },
        );
      }).catch(() => {
        // Expected: either the timeout triggers rollback before commit,
        // or the transaction state is already timed out
      });

      // Data should not be persisted after timeout rollback
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'timeout-1'`;
      expect(rows.length).toBe(0);
    });

    it('runInTransaction should surface the timeout error instead of a second rollback error', async () => {
      let error: Error | undefined;

      await runInContext({}, async () => {
        try {
          await runInTransaction(
            async () => {
              await repository.save({ id: 'timeout-message-1', name: 'will-timeout', value: 42 });
              await new Promise((resolve) => setTimeout(resolve, 150));
            },
            { timeoutMs: 50 },
          );
        } catch (e) {
          error = e as Error;
        }
      });

      expect(error).toBeDefined();
      expect(error?.message).toBe('Transaction has timed out and was rolled back');

      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'timeout-message-1'`;
      expect(rows.length).toBe(0);
    });

    it('should reject operations after timeout', async () => {
      let error: Error | undefined;

      try {
        await runInContext({}, async () => {
          withTransaction({ timeoutMs: 50 });

          await repository.save({ id: 'timeout-reject-1', name: 'first-write', value: 1 });

          // Wait for timeout to fire
          await new Promise((resolve) => setTimeout(resolve, 150));

          // This should fail — transaction is timed out
          await repository.save({ id: 'timeout-reject-2', name: 'after-timeout', value: 2 });
        });
      } catch (e) {
        error = e as Error;
      }

      expect(error).toBeDefined();
      expect(error?.message).toContain('timed out');
    });

    it('should not timeout when timeoutMs is 0 (default)', async () => {
      await runInContext({}, async () => {
        const result = await runInTransaction(async () => {
          await repository.save({ id: 'no-timeout-1', name: 'no-timeout', value: 99 });
          await new Promise((resolve) => setTimeout(resolve, 50));
          return 'done';
        });

        expect(result).toBe('done');
      });

      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'no-timeout-1'`;
      expect(rows.length).toBe(1);
    });

    it('should clear timeout on commit', async () => {
      await runInContext({}, async () => {
        withTransaction({ timeoutMs: 200 });

        await repository.save({ id: 'clear-timeout-1', name: 'fast-commit', value: 10 });
        await commit();

        // Wait past the timeout — should not cause issues since we already committed
        await new Promise((resolve) => setTimeout(resolve, 250));
      });

      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'clear-timeout-1'`;
      expect(rows.length).toBe(1);
    });

    it('commit racing an already-fired timeout rejects cleanly without double-finalizing', async () => {
      let commitError: Error | undefined;

      await runInContext({}, async () => {
        withTransaction({ timeoutMs: 50 });

        await repository.save({ id: 'race-1', name: 'will-rollback', value: 1 });

        // Let the timeout fire (and start its detached, .catch()-guarded
        // rollback) first.
        await new Promise((resolve) => setTimeout(resolve, 120));

        // commit() must observe the timeout, await the in-flight rollback, and
        // reject — rather than double-finalizing the already-released
        // connection or reporting the generic "no active transaction" error.
        try {
          await commit();
        } catch (e) {
          commitError = e as Error;
        }
      });

      expect(commitError).toBeDefined();
      expect(commitError?.message).toBe('Transaction has timed out and was rolled back');
      // Rollback won the race: nothing persisted, and the connection was
      // released exactly once (a double release would have thrown above).
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'race-1'`;
      expect(rows.length).toBe(0);
    });
  });

  describe('delete within transaction', () => {
    it('should rollback deletes', async () => {
      // Insert data outside transaction
      await sql`INSERT INTO tx_test_items (id, name, value) VALUES ('del-1', 'to-delete', 100)`;

      await runInContext({}, async () => {
        withTransaction();

        await repository.delete({ id: 'del-1' });

        await rollback();
      });

      // Data should still be there
      const rows = await sql`SELECT * FROM tx_test_items WHERE id = 'del-1'`;
      expect(rows.length).toBe(1);
    });
  });
});
