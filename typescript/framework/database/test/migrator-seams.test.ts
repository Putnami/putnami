import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import type postgres from 'postgres';
import { MigrationError } from '../src/errors';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import type { SQLDefinition } from '../src/migrations/sql-source';

/**
 * Spec bindings for the Migrator's transaction seams, driven DB-free against a
 * recording stand-in (same shape as migrator-statement-timeout.test.ts): the
 * REAL apply / executeRollback / withLock bodies run, only the connection and
 * the applied-row store are faked, so the exact statement order — BEGIN, body,
 * bookkeeping, COMMIT/ROLLBACK — is assertable without Postgres. The gated
 * integration suite (migrator.test.ts) proves the same contracts against a
 * live server when DATABASE_TEST_BINDINGS is present.
 */

class RecordingSql {
  readonly statements: string[] = [];
  readonly values: unknown[][] = [];
  /** Statement substrings that must reject when executed. */
  readonly failOn: string[] = [];
  /** When false, the rollback bookkeeping DELETE claims no row. */
  claimRows = true;

  private record = (strings: TemplateStringsArray | string[], values: unknown[]): Promise<unknown[]> => {
    let text = strings[0] ?? '';
    for (let i = 0; i < values.length; i++) {
      text += `$${i + 1}${strings[i + 1] ?? ''}`;
    }
    const normalized = text.trim().replace(/\s+/g, ' ');
    this.statements.push(normalized);
    this.values.push(values);
    for (const marker of this.failOn) {
      if (normalized.includes(marker)) {
        return Promise.reject(new Error(`injected failure on: ${marker}`));
      }
    }
    if (/DELETE FROM migration\.migrations.*RETURNING id/.test(normalized)) {
      return Promise.resolve(this.claimRows ? [{ id: 'claimed' }] : []);
    }
    return Promise.resolve([]);
  };

  make(): postgres.ReservedSql {
    const tag = (strings: TemplateStringsArray, ...values: unknown[]) => this.record(strings, values);
    const withUnsafe = tag as unknown as postgres.ReservedSql & {
      unsafe: (query: string) => Promise<unknown[]>;
    };
    withUnsafe.unsafe = (query: string) => {
      const normalized = query.trim().replace(/\s+/g, ' ');
      this.statements.push(normalized);
      for (const marker of this.failOn) {
        if (normalized.includes(marker)) {
          return Promise.reject(new Error(`injected failure on: ${marker}`));
        }
      }
      return Promise.resolve([]);
    };
    return withUnsafe as postgres.ReservedSql;
  }
}

class StubMigrator extends Migrator {
  readonly sql = new RecordingSql();
  private readonly appliedRows: Migration[];

  constructor(definitions: SQLDefinition[], opts: { schema?: string; applied?: Migration[] } = {}) {
    super('seams-test', definitions, opts.schema ?? '');
    this.appliedRows = opts.applied ?? [];
  }

  protected override async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    return await fn(this.sql.make());
  }

  protected override async fetchApplied(): Promise<Migration[]> {
    return this.appliedRows;
  }
}

const def = (name: string, sql: string, down?: string): SQLDefinition => ({
  name,
  sql,
  down,
  namespace: 'seams-test',
});

const migrationRow = (name: string, extra: Partial<Migration> = {}): Migration =>
  ({
    id: `seams-test:${name}`,
    dbName: 'seams-test',
    name,
    hash: 'h',
    executedAt: '2026-01-01T00:00:00.000Z',
    executionTimeMs: 1,
    success: 1,
    ...extra,
  }) as Migration;

describe('Migrator transaction seams (spec)', () => {
  specTest(
    'the body and its success row commit in one transaction',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'atomic-bookkeeping',
      check: 'body-and-success-row-commit-in-one-transaction',
    },
    async () => {
      const migrator = new StubMigrator([def('seams-test/0001_a', 'CREATE TABLE a (id TEXT);')]);
      await migrator.up();

      const stmts = migrator.sql.statements;
      const beginIdx = stmts.indexOf('BEGIN');
      const bodyIdx = stmts.findIndex((s) => s.includes('CREATE TABLE a'));
      const rowIdx = stmts.findIndex((s) => s.includes('INSERT INTO migration.migrations'));
      const commitIdx = stmts.indexOf('COMMIT');

      // One transaction window holds both the DDL and the bookkeeping row:
      // BEGIN < body < success-INSERT < COMMIT, with no other COMMIT between.
      expect(beginIdx).toBeGreaterThanOrEqual(0);
      expect(bodyIdx).toBeGreaterThan(beginIdx);
      expect(rowIdx).toBeGreaterThan(bodyIdx);
      expect(commitIdx).toBeGreaterThan(rowIdx);
      expect(stmts.filter((s) => s === 'COMMIT')).toHaveLength(1);
      expect(stmts.filter((s) => s === 'BEGIN')).toHaveLength(1);
    },
  );

  specTest(
    'a failed body rolls back and the failure row survives outside the transaction',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'failure-is-recorded',
      check: 'a-failed-body-rolls-back-and-the-failure-row-survives',
    },
    async () => {
      const migrator = new StubMigrator([def('seams-test/0001_bad', 'CREATE TABLE broken (id TEXT);')]);
      migrator.sql.failOn.push('CREATE TABLE broken');

      let error: unknown;
      try {
        await migrator.up();
      } catch (e) {
        error = e;
      }

      // The run aborts with a typed error naming the migration.
      expect(error).toBeInstanceOf(MigrationError);
      expect((error as MigrationError).message).toContain('seams-test/0001_bad');

      const stmts = migrator.sql.statements;
      const rollbackIdx = stmts.indexOf('ROLLBACK');
      const failureRowIdx = stmts.findIndex((s) => s.includes('INSERT INTO migration.migrations'));

      // The body's transaction rolled back, and the failure row (success = 0)
      // was recorded AFTER the rollback — outside the rolled-back transaction.
      expect(rollbackIdx).toBeGreaterThanOrEqual(0);
      expect(failureRowIdx).toBeGreaterThan(rollbackIdx);
      expect(stmts).not.toContain('COMMIT');
    },
  );

  specTest(
    'tampered stored rollback SQL is refused before it runs',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'rollback-integrity',
      check: 'tampered-down-sql-is-refused',
    },
    async () => {
      const migrator = new StubMigrator([], {
        applied: [
          migrationRow('seams-test/0001_a', {
            downSql: 'DROP TABLE a;',
            downHash: 'not-the-sha256-of-the-down-sql',
          }),
        ],
      });

      let error: unknown;
      try {
        await migrator.rollback();
      } catch (e) {
        error = e;
      }

      expect(error).toBeInstanceOf(MigrationError);
      expect((error as MigrationError).message).toContain('integrity');
      // Refused before anything ran: no transaction, no down body.
      expect(migrator.sql.statements).not.toContain('BEGIN');
      expect(migrator.sql.statements.some((s) => s.includes('DROP TABLE a'))).toBe(false);
    },
  );

  specTest(
    'an unclaimed state row never runs the down body',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'rollback-integrity',
      check: 'an-unclaimed-row-never-runs-the-down-body',
    },
    async () => {
      const migrator = new StubMigrator([], {
        applied: [migrationRow('seams-test/0001_a', { downSql: 'DROP TABLE a;' })],
      });
      // A concurrent runner already deleted the row: the claim returns nothing.
      migrator.sql.claimRows = false;

      await migrator.rollback();

      const stmts = migrator.sql.statements;
      // The claim was attempted inside the transaction, but with no row claimed
      // the down body never runs — the same migration cannot roll back twice.
      expect(stmts.some((s) => /DELETE FROM migration\.migrations/.test(s))).toBe(true);
      expect(stmts.some((s) => s.includes('DROP TABLE a'))).toBe(false);
      expect(stmts).toContain('COMMIT');
    },
  );

  specTest(
    'every operation runs under the cross-language advisory lock literal',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'single-writer',
      check: 'the-advisory-lock-uses-the-shared-literal',
    },
    async () => {
      // Use the REAL withLock: only the pool acquisition is faked, so the
      // lock/unlock statements and the reserve/release lifecycle are the
      // production ones.
      const recording = new RecordingSql();
      let released = 0;
      const reserved = recording.make() as postgres.ReservedSql & { release: () => void };
      reserved.release = () => {
        released += 1;
      };
      const pool = { reserve: async () => reserved } as unknown as postgres.Sql;

      class LockMigrator extends Migrator {
        protected override async fetchApplied(): Promise<Migration[]> {
          return [];
        }
      }
      const migrator = new LockMigrator('locked-ds', []);
      // The repository() seam owns table bootstrap against a live server; hand
      // it the fake pool directly so withLock's own statements stay real.
      (migrator as unknown as { repository: () => Promise<postgres.Sql> }).repository = async () => pool;

      await migrator.status();

      const stmts = recording.statements;
      // Go and TS hash the SAME literal server-side (hashtext), so concurrent
      // starts serialize across languages: putnami.migration:<datasource>.
      const lockIdx = stmts.findIndex((s) => s.includes('pg_advisory_lock(hashtext($1)::bigint)'));
      const unlockIdx = stmts.findIndex((s) => s.includes('pg_advisory_unlock(hashtext($1)::bigint)'));
      expect(lockIdx).toBeGreaterThanOrEqual(0);
      expect(unlockIdx).toBeGreaterThan(lockIdx);
      // The literal itself is bound as the lock statement's value — the exact
      // string the Go runner hashes, so both languages contend on one key.
      expect(recording.values[lockIdx]).toEqual(['putnami.migration:locked-ds']);
      expect(released).toBe(1);
    },
  );

  specTest(
    'an ambiguous basename fails with the candidate list',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'unambiguous-target',
      check: 'an-ambiguous-basename-fails-with-candidates',
    },
    () => {
      const migrator = new StubMigrator([
        def('app/0001_seed', 'SELECT 1;'),
        def('billing/0001_seed', 'SELECT 1;'),
        def('app/0002_only', 'SELECT 1;'),
      ]);

      // Unique basename resolves to its full name.
      expect(migrator.resolveName('0002_only')).toBe('app/0002_only');
      // Full names resolve as-is.
      expect(migrator.resolveName('billing/0001_seed')).toBe('billing/0001_seed');

      // Ambiguity is an error naming every candidate, never a silent pick.
      let error: unknown;
      try {
        migrator.resolveName('0001_seed');
      } catch (e) {
        error = e;
      }
      expect(error).toBeInstanceOf(MigrationError);
      expect((error as MigrationError).message).toContain('ambiguous');
      expect((error as MigrationError).message).toContain('app/0001_seed');
      expect((error as MigrationError).message).toContain('billing/0001_seed');
    },
  );
});

describe('Migrator advisory-lock literal value', () => {
  it('binds putnami.migration:<datasource> as the lock key', async () => {
    // Companion assertion for the single-writer binding: capture the bound
    // value, not only the statement shape.
    const bound: unknown[] = [];
    const tag = (strings: TemplateStringsArray, ...values: unknown[]) => {
      if (String(strings[0]).includes('pg_advisory_lock')) {
        bound.push(...values);
      }
      return Promise.resolve([]);
    };
    const reserved = tag as unknown as postgres.ReservedSql & { release: () => void };
    (reserved as unknown as { unsafe: (q: string) => Promise<unknown[]> }).unsafe = () => Promise.resolve([]);
    reserved.release = () => {};
    const pool = { reserve: async () => reserved } as unknown as postgres.Sql;

    class LockMigrator extends Migrator {
      protected override async fetchApplied(): Promise<Migration[]> {
        return [];
      }
    }
    const migrator = new LockMigrator('locked-ds', []);
    (migrator as unknown as { repository: () => Promise<postgres.Sql> }).repository = async () => pool;

    await migrator.status();
    expect(bound).toEqual(['putnami.migration:locked-ds']);
  });
});
