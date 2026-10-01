import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import type postgres from 'postgres';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import type { SQLDefinition } from '../src/migrations/sql-source';

/**
 * Migration ordering MUST be plain code-unit / byte order, identical to the
 * Go runner (`defs[i].Name < defs[j].Name`, go/framework/database/
 * migration.go:110). Both runners write the same cross-language state store
 * keyed `${datasource}:${name}`, so the order must be byte-identical across
 * locales and machines — NOT `localeCompare`, which is ICU/locale-dependent.
 *
 * Discriminating vectors (either would sort differently under locale
 * collation somewhere):
 * - `billing-api/…` before `billing/…` — `-` (0x2D) < `/` (0x2F) in byte
 *   order, while en-locale collation puts `billing/…` first.
 * - `app/0002_B` before `app/0002_a` — `B` (0x42) < `a` (0x61) in byte
 *   order, while locale collation orders lowercase before uppercase.
 */

// Scrambled input order; APPLY_ORDER is the byte/code-unit ascending order
// the Go runner produces for the same names.
const SCRAMBLED = ['billing/2026-01-01_x', 'app/0002_a', 'billing-api/2026-01-01_y', 'app/0002_B'];
const APPLY_ORDER = ['app/0002_B', 'app/0002_a', 'billing-api/2026-01-01_y', 'billing/2026-01-01_x'];

/**
 * A reserved-sql stand-in: every query resolves to an empty row set. In
 * executeRollback the DELETE therefore "claims" zero rows, so no down SQL
 * runs and rollback/reset/rollbackTo surface pure ordering.
 */
const stubSql = Object.assign(async (..._args: unknown[]) => [], {
  unsafe: async (_q: string) => [],
}) as unknown as postgres.ReservedSql;

/**
 * DB-free Migrator: overrides the connection-bound seams (withLock /
 * fetchApplied / apply) so ordering runs through the REAL up/rollback/
 * rollbackTo/reset code paths with no Postgres. Mirrors the StubMigrator in
 * migrator-drift.test.ts.
 */
class OrderingMigrator extends Migrator {
  readonly appliedOrder: string[] = [];

  constructor(
    definitions: SQLDefinition[],
    private readonly appliedNames: string[] = [],
  ) {
    super('ordering-test', definitions);
  }

  protected override async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    return await fn(stubSql);
  }

  protected override async fetchApplied(): Promise<Migration[]> {
    return this.appliedNames.map(
      (name) =>
        ({
          id: `ordering-test:${name}`,
          dbName: 'ordering-test',
          name,
          hash: 'h',
          executedAt: '2026-01-01T00:00:00.000Z',
          executionTimeMs: 1,
          success: 1,
          downSql: 'SELECT 1;',
        }) as Migration,
    );
  }

  protected override async apply(_sql: postgres.ReservedSql, def: SQLDefinition): Promise<Migration> {
    this.appliedOrder.push(def.name);
    return {
      id: `ordering-test:${def.name}`,
      dbName: 'ordering-test',
      name: def.name,
      hash: 'h',
      executedAt: '2026-01-01T00:00:00.000Z',
      executionTimeMs: 1,
      success: 1,
    } as Migration;
  }
}

const defs = (names: string[]): SQLDefinition[] =>
  names.map((name) => ({ name, sql: 'SELECT 1;', down: 'SELECT 1;', namespace: 'ordering-test' }));

describe('Migrator ordering is code-unit / byte order (Go parity)', () => {
  specTest(
    'sorts definitions in code-unit ascending order, not locale order',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'byte-order-apply',
      check: 'definitions-sort-in-code-unit-order',
    },
    () => {
      const migrator = new OrderingMigrator(defs(SCRAMBLED));
      expect(migrator.getDefinitions().map((d) => d.name)).toEqual(APPLY_ORDER);
    },
  );

  specTest(
    'applies pending migrations in code-unit ascending order',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'byte-order-apply',
      check: 'pending-migrations-apply-in-code-unit-order',
    },
    async () => {
      const migrator = new OrderingMigrator(defs(SCRAMBLED));
      const result = await migrator.up();
      expect(migrator.appliedOrder).toEqual(APPLY_ORDER);
      expect(result.map((r) => r.name)).toEqual(APPLY_ORDER);
    },
  );

  specTest(
    'reset() rolls back in the exact reverse of the apply order',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'reverse-order-rollback',
      check: 'reset-reverses-the-apply-order',
    },
    async () => {
      const migrator = new OrderingMigrator(defs(SCRAMBLED), SCRAMBLED);
      const rolledBack = await migrator.reset();
      expect(rolledBack.map((r) => r.name)).toEqual([...APPLY_ORDER].reverse());
    },
  );

  it('rollback() picks the code-unit-greatest applied migration', async () => {
    const migrator = new OrderingMigrator(defs(SCRAMBLED), SCRAMBLED);
    const rolledBack = await migrator.rollback();
    expect(rolledBack?.name).toBe('billing/2026-01-01_x');
  });

  specTest(
    'rollbackTo() unwinds only the migrations after the target in code-unit order',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'reverse-order-rollback',
      check: 'rollback-to-unwinds-in-reverse-name-order',
    },
    async () => {
      const migrator = new OrderingMigrator(defs(SCRAMBLED), SCRAMBLED);
      // Descending order is [billing/…, billing-api/…, app/0002_a, app/0002_B];
      // rolling back to app/0002_a must unwind exactly the two billing entries.
      // Locale collation would instead place app/0002_B between app/0002_a and
      // the billing entries and unwind three.
      const rolledBack = await migrator.rollbackTo('app/0002_a');
      expect(rolledBack.map((r) => r.name)).toEqual(['billing/2026-01-01_x', 'billing-api/2026-01-01_y']);
    },
  );
});
