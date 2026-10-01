import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/spectest';
import type postgres from 'postgres';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import type { SQLDefinition } from '../src/migrations/sql-source';

/**
 * A DB-free recording stand-in for a reserved postgres.js connection. Both the
 * tagged-template form (`sql`SELECT ...``) and `sql.unsafe(str)` push the
 * statement text onto `statements`, so a test can assert the exact order in
 * which the Migrator emits its transaction body — no Postgres required. The
 * template form reconstructs the statement by interleaving fragments and a
 * placeholder for each interpolated value, and separately captures those bound
 * values, so an assertion can check both the statement shape and its args.
 */
class RecordingSql {
  readonly statements: string[] = [];
  readonly values: unknown[][] = [];

  private record = (strings: TemplateStringsArray | string[], values: unknown[]): Promise<unknown[]> => {
    let text = strings[0] ?? '';
    for (let i = 0; i < values.length; i++) {
      text += `$${i + 1}${strings[i + 1] ?? ''}`;
    }
    const normalized = text.trim().replace(/\s+/g, ' ');
    this.statements.push(normalized);
    this.values.push(values);
    // The rollback path only runs the down DDL when the bookkeeping DELETE
    // claims a row (RETURNING id), so hand that one back a non-empty result;
    // every other statement is satisfied by an empty row set.
    if (/DELETE FROM migration\.migrations.*RETURNING id/.test(normalized)) {
      return Promise.resolve([{ id: 'claimed' }]);
    }
    return Promise.resolve([]);
  };

  // Emulate the postgres.js callable tag: sql`...` and sql.unsafe(...).
  make(): postgres.ReservedSql {
    const tag = (strings: TemplateStringsArray, ...values: unknown[]) => this.record(strings, values);
    const withUnsafe = tag as unknown as postgres.ReservedSql & {
      unsafe: (query: string) => Promise<unknown[]>;
    };
    withUnsafe.unsafe = (query: string) => {
      this.statements.push(query.trim().replace(/\s+/g, ' '));
      this.values.push([]);
      return Promise.resolve([]);
    };
    return withUnsafe as postgres.ReservedSql;
  }
}

/**
 * Drives the real Migrator.apply / executeRollback against a RecordingSql,
 * bypassing the connection-bound seams (repository/withLock) and the applied-row
 * store so the transaction body can be inspected with no database.
 */
class StubMigrator extends Migrator {
  readonly sql = new RecordingSql();
  private readonly appliedRows: Migration[];

  constructor(definitions: SQLDefinition[], opts: { schema?: string; applied?: Migration[] } = {}) {
    super('to-test', definitions, opts.schema ?? '');
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
  namespace: 'to-test',
});

const migrationRow = (name: string, down: string): Migration =>
  ({
    id: `to-test:${name}`,
    dbName: 'to-test',
    name,
    hash: 'h',
    executedAt: '2026-01-01T00:00:00.000Z',
    executionTimeMs: 1,
    success: 1,
    downSql: down,
  }) as Migration;

describe('Migrator statement_timeout disable', () => {
  specTest(
    'disables statement_timeout inside the transaction before the up DDL',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'unbounded-statement',
      check: 'statement-timeout-is-disabled-transaction-locally-before-the-up-ddl',
    },
    async () => {
      const migrator = new StubMigrator([def('to-test/0001_slow', 'CREATE INDEX CONCURRENTLY slow_idx ON t (c);')]);

      await migrator.up();

      const stmts = migrator.sql.statements;
      const beginIdx = stmts.indexOf('BEGIN');
      const timeoutIdx = stmts.findIndex((s) => s.includes("set_config('statement_timeout'"));
      const ddlIdx = stmts.findIndex((s) => s.includes('CREATE INDEX CONCURRENTLY slow_idx'));

      expect(beginIdx).toBeGreaterThanOrEqual(0);
      expect(timeoutIdx).toBeGreaterThan(beginIdx);
      expect(ddlIdx).toBeGreaterThan(timeoutIdx);
      // Sets the timeout to 0 (unlimited), matching the Go runner; the third arg
      // (is_local) must be true so it reverts at COMMIT and never leaks onto the
      // pooled connection.
      expect(stmts[timeoutIdx]).toBe("SELECT set_config('statement_timeout', '0', true)");
    },
  );

  specTest(
    'disables statement_timeout inside the transaction before the down DDL on rollback',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'unbounded-statement',
      check: 'statement-timeout-is-disabled-before-the-down-ddl',
    },
    async () => {
      const down = 'DROP INDEX CONCURRENTLY slow_idx;';
      const migrator = new StubMigrator([def('to-test/0001_slow', 'CREATE INDEX slow_idx ON t (c);', down)], {
        applied: [migrationRow('to-test/0001_slow', down)],
      });

      await migrator.rollback();

      const stmts = migrator.sql.statements;
      const beginIdx = stmts.indexOf('BEGIN');
      const claimIdx = stmts.findIndex((s) => /DELETE FROM migration\.migrations/.test(s));
      const timeoutIdx = stmts.findIndex((s) => s.includes("set_config('statement_timeout'"));
      const downIdx = stmts.findIndex((s) => s.includes('DROP INDEX CONCURRENTLY slow_idx'));

      expect(beginIdx).toBeGreaterThanOrEqual(0);
      // Mirrors the Go runner: the timeout is disabled only after the bookkeeping
      // row is claimed (inside the `claimed.length > 0` block), and before the
      // down DDL runs.
      expect(claimIdx).toBeGreaterThan(beginIdx);
      expect(timeoutIdx).toBeGreaterThan(claimIdx);
      expect(downIdx).toBeGreaterThan(timeoutIdx);
    },
  );
});
