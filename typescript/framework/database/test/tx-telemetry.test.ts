import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { runInContext } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { specTest } from '@putnami/spectest';
import { setPrimaryDatasource } from '../src/primary-datasource';
import {
  __setTransactionConnectionFactory,
  commit,
  rollback,
  runInTransaction,
  useTxConnection,
  withTransaction,
} from '../src/transaction';
import { UnitOfWork } from '../src/unit-of-work';

/**
 * A deterministic, in-memory stand-in for the postgres.js connection layer that
 * records BEGIN/COMMIT/ROLLBACK per datasource, so the transaction-boundary
 * telemetry can be driven end-to-end without a live Postgres. Mirrors the fake
 * used by unit-of-work.test.ts.
 */
function createFakeBackend() {
  const log: string[] = [];
  const pools = new Map<string, unknown>();

  function makeReserved(key: string) {
    const reserved = ((strings: TemplateStringsArray) => {
      const cmd = String(strings[0]).trim().toUpperCase();
      if (cmd.startsWith('BEGIN')) log.push(`${key}:begin`);
      else if (cmd.startsWith('COMMIT')) log.push(`${key}:commit`);
      else if (cmd.startsWith('ROLLBACK')) log.push(`${key}:rollback`);
      return Promise.resolve([]);
      // biome-ignore lint/suspicious/noExplicitAny: minimal postgres.js reserved-connection fake
    }) as any;
    reserved.release = () => log.push(`${key}:release`);
    return reserved;
  }

  function makePool(key: string) {
    // biome-ignore lint/suspicious/noExplicitAny: minimal postgres.js pool fake
    const pool = (() => Promise.resolve([])) as any;
    pool.reserve = async () => makeReserved(key);
    return pool;
  }

  return {
    log,
    // biome-ignore lint/suspicious/noExplicitAny: returns a postgres.Sql-shaped fake
    factory: async (dbName: string | undefined): Promise<any> => {
      const key = dbName ?? '__default__';
      let pool = pools.get(key);
      if (!pool) {
        pool = makePool(key);
        pools.set(key, pool);
      }
      return pool;
    },
  };
}

let backend: ReturnType<typeof createFakeBackend>;
let collector: TelemetryCollector;
let logger: MemoryLogger;

beforeEach(() => {
  backend = createFakeBackend();
  __setTransactionConnectionFactory(backend.factory);
  setPrimaryDatasource(undefined);
  collector = new TelemetryCollector();
  setCollector(collector);
  logger = new MemoryLogger();
});

afterEach(() => {
  __setTransactionConnectionFactory(undefined);
  setPrimaryDatasource(undefined);
  setCollector(undefined);
});

afterAll(() => {
  __setTransactionConnectionFactory(undefined);
  setCollector(undefined);
});

/**
 * The `database` group of a record, the way the JSON sink renders it: the lone
 * object param is flattened over the entry, so the boundary fields live under
 * one nested key (contract: "Domain groups").
 */
function databaseGroup(entry: { data?: unknown[] } | undefined): Record<string, unknown> | undefined {
  return (entry?.data?.[0] as Record<string, unknown> | undefined)?.['database'] as Record<string, unknown> | undefined;
}

/** All metric names emitted across counters, histograms and gauges. */
function allMetricNames(): string[] {
  const names: string[] = [];
  for (const bucket of collector.drainAll()) {
    names.push(...Object.keys(bucket.counters), ...Object.keys(bucket.histograms), ...Object.keys(bucket.gauges));
  }
  return names;
}

describe('Transaction telemetry — commit', () => {
  it('emits the committed boundary metrics + a "transaction committed" span', async () => {
    await runInContext({ logger } as never, async () => {
      await runInTransaction(async () => {
        await useTxConnection('db1', 'write');
      });
    });

    const bucket = collector.drainAll()[0];
    expect(bucket.counters['sql.tx.outcome.committed']).toBe(1);
    expect(bucket.histograms['sql.tx.duration']).toBeDefined();
    expect(bucket.histograms['sql.tx.retries'].sum).toBe(0);
    expect(bucket.counters['sql.tx.outcome.rolled-back']).toBeUndefined();

    const span = logger.entries.find((e) => e.message === 'transaction committed');
    expect(span).toBeDefined();
    expect(span?.logger).toBe('database');
    // committed maps onto the contract's shared outcome vocabulary, and the
    // record carries both duration units.
    expect(databaseGroup(span)).toEqual({
      datasource: 'db1',
      outcome: 'success',
      durationMs: expect.any(Number),
      durationUs: expect.any(Number),
      retries: 0,
    });
  });

  it('emits nothing when only reads happened (no real transaction)', async () => {
    await runInContext({ logger } as never, async () => {
      withTransaction();
      // No write → no reserved connection → no BEGIN/COMMIT.
      await commit();
    });

    expect(collector.isEmpty()).toBe(true);
  });
});

describe('Transaction telemetry — rollback', () => {
  specTest(
    'emits the rolled-back boundary metrics with the classified SQLSTATE cause',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'secret-free-telemetry',
      check: 'rollback-telemetry-carries-a-classified-cause-code-only',
    },
    async () => {
      const driverErr = Object.assign(new Error('serialization failure'), { code: '40001' });

      await runInContext({ logger } as never, async () => {
        await runInTransaction(async () => {
          await useTxConnection('db1', 'write');
          throw driverErr;
        }).catch(() => {});
      });

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['sql.tx.outcome.rolled-back']).toBe(1);
      expect(bucket.counters['sql.tx.rollback.40001']).toBe(1);
      expect(bucket.histograms['sql.tx.duration']).toBeDefined();

      const span = logger.entries.find((e) => e.message === 'transaction rolled back');
      expect(span?.level).toBe('warn');
      expect(databaseGroup(span)).toEqual({
        datasource: 'db1',
        outcome: 'failure',
        durationMs: expect.any(Number),
        durationUs: expect.any(Number),
        retries: 0,
        rollbackCause: '40001',
      });
      // A rollback record carries the classified cause ONLY — never an error.
      expect(span?.error).toBeUndefined();
    },
  );

  it('records the explicit-rollback sentinel for a cause-less rollback', async () => {
    await runInContext({ logger } as never, async () => {
      withTransaction();
      await useTxConnection('db1', 'write');
      await rollback();
    });

    const bucket = collector.drainAll()[0];
    expect(bucket.counters['sql.tx.rollback.explicit-rollback']).toBe(1);
  });

  it('threads the request outcome through UnitOfWork.finalize into the classified cause', async () => {
    const driverErr = Object.assign(new Error('serialize'), { code: '40001' });

    await runInContext({ logger } as never, async () => {
      const uow = new UnitOfWork();
      uow.begin();
      await useTxConnection('db1', 'write');
      await uow.finalize(driverErr);
    });

    const bucket = collector.drainAll()[0];
    expect(bucket.counters['sql.tx.outcome.rolled-back']).toBe(1);
    expect(bucket.counters['sql.tx.rollback.40001']).toBe(1);
  });
});

describe('Transaction telemetry — secret safety', () => {
  const SECRET = 'tok_LEAK_hunter2_pw';

  specTest(
    'never leaks a secret from the rollback error into any metric name or log line',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'secret-free-telemetry',
      check: 'no-secret-reaches-metrics-or-logs',
    },
    async () => {
      // A driver error carrying a bound value in its message — exactly the shape
      // that would leak if the cause attribute were err.message.
      const driverErr = Object.assign(new Error(`duplicate key value: token=(${SECRET}) already exists`), {
        code: '23505',
        detail: `Key (token)=(${SECRET}) already exists.`,
      });

      // Sanity: the raw error genuinely carries the secret, so the test is real.
      expect(driverErr.message).toContain(SECRET);

      await runInContext({ logger } as never, async () => {
        await runInTransaction(async () => {
          await useTxConnection('db1', 'write');
          throw driverErr;
        }).catch(() => {});
      });

      // The cause classified to the SQLSTATE, and NO emitted metric name carries
      // the secret.
      const names = allMetricNames();
      expect(names).toContain('sql.tx.rollback.23505');
      for (const name of names) {
        expect(name).not.toContain(SECRET);
      }

      // And no captured log line carries it either.
      const serializedLogs = JSON.stringify(logger.entries);
      expect(serializedLogs).not.toContain(SECRET);
      // The rolled-back span exists and carries only the classified code.
      const span = logger.entries.find((e) => e.message === 'transaction rolled back');
      expect(databaseGroup(span)?.['rollbackCause']).toBe('23505');
    },
  );
});
