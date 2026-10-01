// biome-ignore-all lint/suspicious/noConsole: the JsonSink writes to console.log; these tests intercept it
import { afterEach, describe, expect, it } from 'bun:test';
import { MigrationRegistry } from '@putnami/migration';
import { JsonSink, Logger, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import type postgres from 'postgres';
import type { Migration } from '../src/migrations/migration.entity';
import { Migrator } from '../src/migrations/migrator';
import { SQLRunner } from '../src/migrations/sql-runner';
import type { SQLDefinition } from '../src/migrations/sql-source';
import { recordQuery, recordQueryError, recordSlowQuery, recordTransaction } from '../src/observability';

/**
 * These tests pin the database boundary's log-record contract
 * (`protocols/logging/conformance`): the nested `database` / `migration` groups
 * with camelCase keys and both duration units, the closed outcome vocabulary,
 * the constant messages, the severity policy (a failed query is a WARNING that
 * propagates; a failed migration is an ERROR), the pinned logger names, and —
 * load-bearing — the failure's real error landing in the record's structured
 * `error` field instead of being silently dropped.
 */

afterEach(() => {
  resetDefaultLogger();
});

/** Capture records emitted by the code under test. */
function captureRecords(): MemoryLogger {
  const logger = new MemoryLogger();
  setRootLogger(logger);
  return logger;
}

/** The `database` group of a record, as the JSON sink flattens it. */
function databaseGroup(entry: { data?: unknown[] } | undefined): Record<string, unknown> | undefined {
  return (entry?.data?.[0] as Record<string, unknown> | undefined)?.['database'] as Record<string, unknown> | undefined;
}

/** The `migration` group of a record, as the JSON sink flattens it. */
function migrationGroup(entry: { data?: unknown[] } | undefined): Record<string, unknown> | undefined {
  return (entry?.data?.[0] as Record<string, unknown> | undefined)?.['migration'] as
    | Record<string, unknown>
    | undefined;
}

/**
 * Run `fn` with the real {@link JsonSink} installed and return the parsed JSON
 * lines it printed. This is the only way to assert what actually reaches a log
 * aggregator: the sink drops reserved keys (including `error`) when flattening a
 * data object, so a record's error is only visible here if it travelled as a log
 * param.
 */
function captureJsonLines(fn: () => void): Record<string, unknown>[] {
  const lines: string[] = [];
  const original = console.log;
  console.log = (line?: unknown) => {
    lines.push(String(line));
  };
  try {
    setRootLogger(new Logger([new JsonSink()]));
    fn();
  } finally {
    console.log = original;
  }
  return lines.map((line) => JSON.parse(line) as Record<string, unknown>);
}

describe('query records', () => {
  it('emits "query executed" at debug under the pinned database logger', () => {
    const logger = captureRecords();

    recordQuery({ operation: 'find', table: 'users', duration: 12, rowCount: 3, datasource: 'orders' });

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    expect(entry?.level).toBe('debug');
    // Pinned logger name of the contract: dots, never colons.
    expect(entry?.logger).toBe('database');
    // The message is a constant — no identifier is interpolated into it.
    expect(entry?.message).toBe('query executed');
    expect(databaseGroup(entry)).toEqual({
      operation: 'find',
      table: 'users',
      datasource: 'orders',
      durationMs: 12,
      durationUs: 12_000,
      outcome: 'success',
      rowCount: 3,
    });
  });

  it('labels a record with the default datasource when the repository declares none', () => {
    const logger = captureRecords();

    recordQuery({ operation: 'count', table: 'users', duration: 1 });

    expect(databaseGroup(logger.entries[0])?.['datasource']).toBe('default');
  });

  it('derives durationUs from the sub-millisecond measurement', () => {
    const logger = captureRecords();

    // performance.now() yields fractional milliseconds: a 420µs query must not
    // report 0µs (which a Date.now()-based measurement would).
    recordQuery({ operation: 'exists', table: 'users', duration: 0.42 });

    const group = databaseGroup(logger.entries[0]);
    expect(group?.['durationMs']).toBe(0);
    expect(group?.['durationUs']).toBe(420);
  });

  it('emits "query failed" at WARN — the data layer reports and re-throws', () => {
    const logger = captureRecords();

    recordQueryError('save', 'users', 45, new Error('duplicate key value'), 'orders');

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    // WARNING, not ERROR: the boundary that actually fails owns ERROR, exactly
    // once, so a retried-and-recovered query is not error-level noise.
    expect(entry?.level).toBe('warn');
    expect(entry?.logger).toBe('database');
    expect(entry?.message).toBe('query failed');
    expect(databaseGroup(entry)).toEqual({
      operation: 'save',
      table: 'users',
      datasource: 'orders',
      durationMs: 45,
      durationUs: 45_000,
      outcome: 'failure',
    });
  });

  it('carries the database error in the record’s structured error field', () => {
    const logger = captureRecords();

    recordQueryError('save', 'users', 45, new Error('duplicate key value'));

    const entry = logger.entries[0];
    expect(entry?.error?.name).toBe('Error');
    expect(entry?.error?.message).toBe('duplicate key value');
    // The error is NOT a group field: `error` is framework-reserved and would be
    // dropped by the sink (see the JSON regression below).
    expect(databaseGroup(entry)).not.toHaveProperty('error');
  });

  it('REGRESSION: the failed query’s error reaches the JSON output', () => {
    // The record used to pass `error: <string>` inside its data object. The JSON
    // sink SKIPS reserved keys (`error` among them) when flattening that object,
    // so the database error message never reached the log aggregator at all.
    const records = captureJsonLines(() => {
      recordQueryError('save', 'users', 45, new Error('connection refused'), 'orders');
    });

    expect(records).toHaveLength(1);
    const [record = {}] = records;
    expect(record['severity']).toBe('WARNING');
    expect(record['message']).toBe('query failed');
    expect(record['logger']).toBe('database');
    expect((record['error'] as Record<string, unknown>)?.['message']).toBe('connection refused');
    // The domain fields still land under the one nested group.
    expect((record['database'] as Record<string, unknown>)?.['outcome']).toBe('failure');
    // …and never as an un-flattened `data` array.
    expect(record).not.toHaveProperty('data');
  });

  it('coerces a thrown non-Error into a real error field', () => {
    const records = captureJsonLines(() => {
      recordQueryError('find', 'orders', 50, 'string error');
    });

    const [record = {}] = records;
    expect((record['error'] as Record<string, unknown>)?.['message']).toBe('string error');
    expect((record['database'] as Record<string, unknown>)?.['operation']).toBe('find');
    expect(record).not.toHaveProperty('data');
  });

  it('emits "slow query" at warn with the threshold as a field', () => {
    const logger = captureRecords();

    recordSlowQuery({ operation: 'find', table: 'users', duration: 312, datasource: 'orders' }, 200);

    const entry = logger.entries[0];
    expect(entry?.level).toBe('warn');
    expect(entry?.message).toBe('slow query');
    // Neither the duration nor the threshold is interpolated into the message.
    expect(entry?.message).not.toContain('312');
    expect(databaseGroup(entry)).toEqual({
      operation: 'find',
      table: 'users',
      datasource: 'orders',
      durationMs: 312,
      durationUs: 312_000,
      // The query itself SUCCEEDED — slowness is not a failure.
      outcome: 'success',
      thresholdMs: 200,
    });
  });
});

describe('transaction records', () => {
  it('keeps the group closed on commit and always reports a rollback cause', () => {
    const logger = captureRecords();

    recordTransaction({ datasource: 'orders', outcome: 'committed', duration: 2 });
    // A rollback with NO classified cause: the field must still be present, with
    // the same `unknown` catch-all the Go emitter uses.
    recordTransaction({ datasource: 'orders', outcome: 'rolled-back', duration: 1 });

    const commit = logger.entries.find((e) => e.message === 'transaction committed');
    expect(commit?.level).toBe('debug');
    expect(databaseGroup(commit)).toEqual({
      datasource: 'orders',
      outcome: 'success',
      durationMs: 2,
      durationUs: 2000,
      retries: 0,
    });

    const rolledBack = logger.entries.find((e) => e.message === 'transaction rolled back');
    expect(rolledBack?.level).toBe('warn');
    expect(databaseGroup(rolledBack)).toEqual({
      datasource: 'orders',
      outcome: 'failure',
      durationMs: 1,
      durationUs: 1000,
      retries: 0,
      rollbackCause: 'unknown',
    });
    // A rollback record never carries an error — only the classified cause.
    expect(rolledBack?.error).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Migration terminal records — driven through the real Migrator with a DB-free
// recording connection (same seam as migrator-statement-timeout.test.ts), so no
// Postgres is required and the integration-gated suites stay untouched.
// ---------------------------------------------------------------------------

class RecordingSql {
  readonly statements: string[] = [];
  /** Migration bodies that must reject, keyed by their exact text. */
  readonly failures = new Map<string, Error>();

  private record = (strings: TemplateStringsArray | string[], values: unknown[]): Promise<unknown[]> => {
    let text = strings[0] ?? '';
    for (let i = 0; i < values.length; i++) {
      text += `$${i + 1}${strings[i + 1] ?? ''}`;
    }
    this.statements.push(text.trim().replace(/\s+/g, ' '));
    return Promise.resolve([]);
  };

  make(): postgres.ReservedSql {
    const tag = (strings: TemplateStringsArray, ...values: unknown[]) => this.record(strings, values);
    const withUnsafe = tag as unknown as postgres.ReservedSql & { unsafe: (query: string) => Promise<unknown[]> };
    withUnsafe.unsafe = (query: string) => {
      const failure = this.failures.get(query);
      if (failure) {
        return Promise.reject(failure);
      }
      this.statements.push(query.trim().replace(/\s+/g, ' '));
      return Promise.resolve([]);
    };
    return withUnsafe as postgres.ReservedSql;
  }
}

class StubMigrator extends Migrator {
  readonly sql = new RecordingSql();

  constructor(definitions: SQLDefinition[]) {
    super('orders', definitions, '');
  }

  protected override async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    return await fn(this.sql.make());
  }

  protected override async fetchApplied(): Promise<Migration[]> {
    return [];
  }
}

const def = (name: string, sql: string): SQLDefinition => ({ name, sql, namespace: 'orders' });

describe('migration terminal records', () => {
  it('emits one "migration applied" INFO record per applied migration', async () => {
    const logger = captureRecords();
    const migrator = new StubMigrator([
      def('orders/0001_conformance', 'CREATE TABLE a (id int);'),
      def('orders/0002_more', 'CREATE TABLE b (id int);'),
    ]);

    await migrator.up();

    const applied = logger.entries.filter((e) => e.message === 'migration applied');
    expect(applied).toHaveLength(2);
    expect(applied[0]?.level).toBe('info');
    // Pinned logger name shared by the migrator, the runner, and the registry.
    expect(applied[0]?.logger).toBe('database.migration');
    expect(migrationGroup(applied[0])).toEqual({
      name: 'orders/0001_conformance',
      datasource: 'orders',
      durationMs: expect.any(Number),
      outcome: 'success',
    });
    expect(migrationGroup(applied[1])?.['name']).toBe('orders/0002_more');
    expect(logger.entries.filter((e) => e.message === 'migration failed')).toHaveLength(0);
  });

  it('emits one "migration failed" ERROR record with the cause, and still throws', async () => {
    const logger = captureRecords();
    const body = 'CREATE TABLE broken (';
    const migrator = new StubMigrator([def('orders/0002_conformance_failing', body)]);
    migrator.sql.failures.set(body, new Error('syntax error at end of input'));

    await expect(migrator.up()).rejects.toThrow('syntax error at end of input');

    const failed = logger.entries.filter((e) => e.message === 'migration failed');
    expect(failed).toHaveLength(1);
    expect(failed[0]?.level).toBe('error');
    expect(failed[0]?.logger).toBe('database.migration');
    // The raw cause travels structured; the message stays a constant.
    expect(failed[0]?.error?.message).toBe('syntax error at end of input');
    expect(migrationGroup(failed[0])).toEqual({
      name: 'orders/0002_conformance_failing',
      datasource: 'orders',
      durationMs: expect.any(Number),
      outcome: 'failure',
    });
    expect(logger.entries.filter((e) => e.message === 'migration applied')).toHaveLength(0);
  });

  it('groups the SQL runner’s summary record under the migration key', async () => {
    const logger = captureRecords();
    const runner = new SQLRunner({ registry: new MigrationRegistry(), autoApply: true });
    const row = {
      id: 'orders:orders/0001_conformance',
      dbName: 'orders',
      name: 'orders/0001_conformance',
      hash: 'h',
      executedAt: '2026-01-01T00:00:00.000Z',
      executionTimeMs: 1,
      success: 1,
    } as Migration;
    // Materialization needs a live connection; inject one already-materialized
    // Migrator so the aggregate summary record is reachable without a database.
    (runner as unknown as { migrators: Map<string, { up(): Promise<Migration[]> }> }).migrators = new Map([
      ['orders', { up: async () => [row] }],
    ]);

    await runner.apply({ force: true });

    const summary = logger.entries.find((e) => e.message === 'migrations applied');
    expect(summary?.logger).toBe('database.migration');
    expect(migrationGroup(summary)).toEqual({ count: 1, datasources: 1 });
  });

  it('groups the migrator’s bookkeeping records under the migration key', async () => {
    const logger = captureRecords();
    const migrator = new StubMigrator([def('orders/0001_conformance', 'CREATE TABLE a (id int);')]);

    await migrator.up();

    const executing = logger.entries.find((e) => e.message === 'executing migration');
    expect(executing?.logger).toBe('database.migration');
    expect(migrationGroup(executing)?.['name']).toBe('orders/0001_conformance');
    expect(migrationGroup(executing)?.['datasource']).toBe('orders');
  });
});
