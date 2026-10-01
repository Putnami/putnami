import { describe, expect, it } from 'bun:test';
import { sha256Hex } from '@putnami/migration';
import { specTest } from '@putnami/spectest';
import type postgres from 'postgres';
import { MigrationError } from '../src/errors';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import type { SQLDefinition } from '../src/migrations/sql-source';

/**
 * A DB-free Migrator whose store is an in-memory list of already-applied rows.
 * It overrides the three connection-bound seams (withLock / fetchApplied /
 * apply) so upTo() runs its drift check against canned rows with no Postgres.
 * The drift comparison itself uses the real sha256Hex, so the test exercises
 * the production hashing path.
 */
class StubMigrator extends Migrator {
  applied: { name: string; hash: string }[];
  readonly newlyApplied: string[] = [];

  constructor(definitions: SQLDefinition[], applied: { name: string; hash: string }[]) {
    super('drift-test', definitions);
    this.applied = applied;
  }

  protected override async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    // upTo never touches the connection on the drift path (it throws before
    // any query) and only the unchanged/pending paths call apply, which we
    // also stub — so a do-nothing reserved-sql stand-in is sufficient.
    return await fn({} as postgres.ReservedSql);
  }

  protected override async fetchApplied(): Promise<Migration[]> {
    return this.applied.map(
      (row) =>
        ({
          id: `drift-test:${row.name}`,
          dbName: 'drift-test',
          name: row.name,
          hash: row.hash,
          executedAt: '2026-01-01T00:00:00.000Z',
          executionTimeMs: 1,
          success: 1,
        }) as Migration,
    );
  }

  protected override async apply(_sql: postgres.ReservedSql, def: SQLDefinition): Promise<Migration> {
    this.newlyApplied.push(def.name);
    const hash = await sha256Hex(def.sql);
    this.applied.push({ name: def.name, hash });
    return {
      id: `drift-test:${def.name}`,
      dbName: 'drift-test',
      name: def.name,
      hash,
      executedAt: '2026-01-01T00:00:00.000Z',
      executionTimeMs: 1,
      success: 1,
    } as Migration;
  }
}

const def = (name: string, sql: string): SQLDefinition => ({ name, sql, namespace: 'drift-test' });

describe('Migrator.upTo body-drift detection', () => {
  specTest(
    'throws when an already-applied migration body has changed',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'body-drift-fails',
      check: 'a-drifted-applied-body-fails-the-run',
    },
    async () => {
      const original = 'CREATE TABLE t (id int);';
      const storedHash = await sha256Hex(original);
      // The committed definition now carries DIFFERENT sql than what was applied.
      const migrator = new StubMigrator(
        [def('drift-test/0001_t', 'CREATE TABLE t (id int, name text);')],
        [{ name: 'drift-test/0001_t', hash: storedHash }],
      );

      await expect(migrator.up()).rejects.toBeInstanceOf(MigrationError);
      expect(migrator.newlyApplied).toEqual([]);
    },
  );

  specTest(
    'names the migration and both hashes in the drift error',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'body-drift-fails',
      check: 'the-drift-error-names-the-migration-and-both-hashes',
    },
    async () => {
      const storedHash = await sha256Hex('SELECT 1;');
      const migrator = new StubMigrator(
        [def('drift-test/0001_t', 'SELECT 2;')],
        [{ name: 'drift-test/0001_t', hash: storedHash }],
      );
      const currentHash = await sha256Hex('SELECT 2;');

      let caught: unknown;
      try {
        await migrator.up();
      } catch (e) {
        caught = e;
      }
      expect(caught).toBeInstanceOf(MigrationError);
      const message = (caught as Error).message;
      expect(message).toContain('drift-test/0001_t');
      expect(message).toContain(storedHash);
      expect(message).toContain(currentHash);
      expect(message).toMatch(/changed since it was applied/);
    },
  );

  it('does not throw when the applied migration body is unchanged', async () => {
    const sql = 'CREATE TABLE t (id int);';
    const storedHash = await sha256Hex(sql);
    const migrator = new StubMigrator(
      [def('drift-test/0001_t', sql)],
      [{ name: 'drift-test/0001_t', hash: storedHash }],
    );

    const result = await migrator.up();
    expect(result).toEqual([]); // nothing new applied
    expect(migrator.newlyApplied).toEqual([]);
  });

  it('still applies a genuinely pending migration that sits after an unchanged applied one', async () => {
    const appliedSql = 'CREATE TABLE a (id int);';
    const storedHash = await sha256Hex(appliedSql);
    const migrator = new StubMigrator(
      [def('drift-test/0001_a', appliedSql), def('drift-test/0002_b', 'CREATE TABLE b (id int);')],
      [{ name: 'drift-test/0001_a', hash: storedHash }],
    );

    const result = await migrator.up();
    expect(migrator.newlyApplied).toEqual(['drift-test/0002_b']);
    expect(result.map((r) => r.name)).toEqual(['drift-test/0002_b']);
  });

  it('detects drift on the applied migration even when a later one is pending', async () => {
    const migrator = new StubMigrator(
      [
        def('drift-test/0001_a', 'CREATE TABLE a (id int, extra text);'), // body changed
        def('drift-test/0002_b', 'CREATE TABLE b (id int);'),
      ],
      [{ name: 'drift-test/0001_a', hash: await sha256Hex('CREATE TABLE a (id int);') }],
    );

    await expect(migrator.up()).rejects.toBeInstanceOf(MigrationError);
    // It must abort on the drift before applying the pending sibling.
    expect(migrator.newlyApplied).toEqual([]);
  });
});
