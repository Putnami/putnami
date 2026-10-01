import { afterEach, describe, expect, it } from 'bun:test';
import { resetDefaultLogger, runInContext, setRootLogger } from '@putnami/runtime';
import { assertRecord, findCase, findRecord, loadCases, MemoryLogger } from '@putnami/runtime/testing';
import type postgres from 'postgres';
import { closeAllDatabases, database } from '../src/factory';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import type { SQLDefinition } from '../src/migrations/sql-source';
import { Column, Key, Repository, runInTransaction, Table } from '../src/index';
import { provision, type ProvisionResult } from '../src/test-provider';

/**
 * This file executes the database AND migration boundaries' cases of the canonical
 * cross-runtime log corpus (`protocols/logging/conformance`) against the records
 * the REAL repository / transaction runner / Migrator emit through the REAL JSON
 * sink path (`buildJsonRecord`). Its Go twin is
 * `go/framework/database/logging_cross_language_test.go`.
 *
 * The database cases need a live Postgres and use the same gating as the
 * transaction conformance corpus (`DATABASE_TEST_BINDINGS`; skipped without it).
 * The migration cases drive the real Migrator over a DB-free recording connection,
 * so they run in the ordinary unit gate — a migration record is a contract too,
 * and waiting for a Postgres-enabled run to catch drift in it would be the slow
 * way to find out.
 */

const databaseCases = loadCases('database');
const migrationCases = loadCases('migration');
const hasBinding = !!process.env['DATABASE_TEST_BINDINGS']?.trim();

const TABLE = 'logging_conformance_orders';

afterEach(() => {
  resetDefaultLogger();
});

// ---------------------------------------------------------------------------
// Database boundary — live Postgres (integration-gated)
// ---------------------------------------------------------------------------

describe('database logging conformance (live)', () => {
  it.skipIf(!hasBinding)('emits the corpus records for queries and transactions', async () => {
    const result: ProvisionResult = await provision();
    try {
      const names = Object.keys(result.binding.databases ?? {}).sort();
      expect(names.length).toBeGreaterThan(0);
      const dsName = names[0] as string;

      const sql = await database(dsName);
      await sql.unsafe(`DROP TABLE IF EXISTS "${TABLE}"`);
      await sql.unsafe(`CREATE TABLE "${TABLE}" (id TEXT PRIMARY KEY, name TEXT)`);
      // Enough rows that reading them all crosses the 1ms slow-query threshold
      // deterministically (a single-row read would race the threshold).
      await sql.unsafe(
        `INSERT INTO "${TABLE}" (id, name) SELECT g::text, repeat('n', 200) FROM generate_series(1, 5000) g`,
      );

      const table = Table(TABLE, { id: Key(String), name: Column(String) }, { db: dsName });
      const repo = new Repository(table);

      // db.query.success — the smallest read the repository offers. No request
      // scope carries a traceId: the corpus requires database records to have none.
      let memory = new MemoryLogger();
      setRootLogger(memory);
      let want = findCase(databaseCases, 'db.query.success');
      await runInContext({}, async () => {
        await repo.count();
      });
      assertRecord(findRecord(memory.entries, want), want);

      // db.query.failure — a repository bound to a relation that does not exist.
      // The data layer reports WARNING and RE-THROWS; the boundary owns ERROR.
      memory = new MemoryLogger();
      setRootLogger(memory);
      want = findCase(databaseCases, 'db.query.failure');
      const missing = new Repository(Table('logging_conformance_missing', { id: Key(String) }, { db: dsName }));
      await runInContext({}, async () => {
        await expect(missing.count()).rejects.toThrow();
      });
      assertRecord(findRecord(memory.entries, want), want);

      // db.query.slow — the query SUCCEEDS but crosses the configured threshold.
      // The threshold is normally resolved from config once and cached on the
      // repository; setting the cache directly is the narrowest seam that drives
      // the real observe() → recordSlowQuery() path.
      memory = new MemoryLogger();
      setRootLogger(memory);
      want = findCase(databaseCases, 'db.query.slow');
      const slowRepo = new Repository(table);
      (slowRepo as unknown as { _slowQueryThresholdMs: number })._slowQueryThresholdMs = 1;
      await runInContext({}, async () => {
        await slowRepo.find({});
      });
      assertRecord(findRecord(memory.entries, want), want);

      // db.tx.commit — a real transaction boundary with one WRITE (a read-only
      // transaction deliberately never opens a connection, so it has no boundary
      // to report) that commits.
      memory = new MemoryLogger();
      setRootLogger(memory);
      want = findCase(databaseCases, 'db.tx.commit');
      await runInContext({}, async () => {
        await runInTransaction(async () => {
          await repo.save({ id: 'tx-commit', name: 'committed' });
        });
      });
      assertRecord(findRecord(memory.entries, want), want);

      // db.tx.rollback — the callback throws, so the boundary rolls back and
      // reports the CLASSIFIED cause only, never the error text.
      memory = new MemoryLogger();
      setRootLogger(memory);
      want = findCase(databaseCases, 'db.tx.rollback');
      await runInContext({}, async () => {
        await expect(
          runInTransaction(async () => {
            await repo.save({ id: 'tx-rollback', name: 'rolled back' });
            throw new Error('conformance rollback');
          }),
        ).rejects.toThrow('conformance rollback');
      });
      const rollback = findRecord(memory.entries, want);
      assertRecord(rollback, want);
      expect(JSON.stringify(rollback)).not.toContain('conformance rollback');

      await sql.unsafe(`DROP TABLE IF EXISTS "${TABLE}"`);
    } finally {
      await closeAllDatabases();
      await result.cleanup();
    }
  });
});

// ---------------------------------------------------------------------------
// Migration boundary — real Migrator over a DB-free recording connection
// (the same seam database-logging.test.ts uses), so no Postgres is required.
// ---------------------------------------------------------------------------

class RecordingSql {
  /** Migration bodies that must reject, keyed by their exact text. */
  readonly failures = new Map<string, Error>();

  make(): postgres.ReservedSql {
    // Bookkeeping statements (the migration table, the applied-rows insert) run
    // through the template tag and resolve empty; only migration BODIES go through
    // `unsafe`, which is where a failure can be injected.
    const tag = () => Promise.resolve([]);
    const withUnsafe = tag as unknown as postgres.ReservedSql & { unsafe: (query: string) => Promise<unknown[]> };
    withUnsafe.unsafe = (query: string) => {
      const failure = this.failures.get(query);
      return failure ? Promise.reject(failure) : Promise.resolve([]);
    };
    return withUnsafe as postgres.ReservedSql;
  }
}

class StubMigrator extends Migrator {
  readonly sql = new RecordingSql();

  constructor(definitions: SQLDefinition[]) {
    super('primary', definitions, '');
  }

  protected override async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    return await fn(this.sql.make());
  }

  protected override async fetchApplied(): Promise<Migration[]> {
    return [];
  }
}

/**
 * The corpus pins bare migration names (`0001_conformance`), so the definitions
 * carry the name verbatim — exactly what the Migrator records.
 */
const def = (name: string, sql: string): SQLDefinition => ({ name, sql, namespace: 'conformance' });

describe('migration logging conformance', () => {
  it('emits the corpus record for an applied migration', async () => {
    const want = findCase(migrationCases, 'migration.applied');
    const memory = new MemoryLogger();
    setRootLogger(memory);

    await new StubMigrator([def('0001_conformance', 'CREATE TABLE conformance (id int);')]).up();

    assertRecord(findRecord(memory.entries, want), want);
  });

  it('emits the corpus record for a failed migration, and still throws', async () => {
    const want = findCase(migrationCases, 'migration.failed');
    const memory = new MemoryLogger();
    setRootLogger(memory);

    const body = 'CREATE TABLE broken (';
    const migrator = new StubMigrator([def('0002_conformance_failing', body)]);
    migrator.sql.failures.set(body, new Error('syntax error at end of input'));

    await expect(migrator.up()).rejects.toThrow('syntax error at end of input');

    assertRecord(findRecord(memory.entries, want), want);
  });
});
