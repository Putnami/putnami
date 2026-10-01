import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { ContainerContext, provide, runInContext } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY, useContainer } from '@putnami/runtime/inject';
import { specTest } from '@putnami/spectest';
import { Column, Key, Repository, Table } from '../src/index';
import { setPrimaryDatasource } from '../src/primary-datasource';
import {
  __setTransactionConnectionFactory,
  cleanupTransaction,
  commit,
  isTransactionActive,
  runInTransaction,
  useTxConnection,
  withTransaction,
} from '../src/transaction';
import { UnitOfWork } from '../src/unit-of-work';
import { UnitOfWorkMiddleware } from '../src/unit-of-work.middleware';

/**
 * A deterministic, in-memory stand-in for the postgres.js connection layer. It
 * records BEGIN/COMMIT/ROLLBACK/reserve/release per datasource so tests can
 * assert on the transaction boundary without a live Postgres. Installed via the
 * `@internal` connection-factory seam on the transaction module.
 */
function createFakeBackend() {
  const log: string[] = [];
  const failCommitFor = new Set<string>();
  const pools = new Map<string, unknown>();
  // The single reserved connection per datasource, so identity assertions
  // ("a new'd repo joins the same tx") can compare object references.
  const reservedByKey = new Map<string, unknown>();

  function makeReserved(key: string) {
    const reserved = ((strings: TemplateStringsArray) => {
      const cmd = String(strings[0]).trim().toUpperCase();
      if (cmd.startsWith('BEGIN')) {
        log.push(`${key}:begin`);
      } else if (cmd.startsWith('COMMIT')) {
        if (failCommitFor.has(key)) {
          log.push(`${key}:commit-fail`);
          return Promise.reject(new Error(`commit failed for ${key}`));
        }
        log.push(`${key}:commit`);
      } else if (cmd.startsWith('ROLLBACK')) {
        log.push(`${key}:rollback`);
      }
      return Promise.resolve([]);
      // biome-ignore lint/suspicious/noExplicitAny: minimal postgres.js reserved-connection fake
    }) as any;
    reserved.release = () => log.push(`${key}:release`);
    return reserved;
  }

  function makePool(key: string) {
    // biome-ignore lint/suspicious/noExplicitAny: minimal postgres.js pool fake
    const pool = (() => Promise.resolve([])) as any;
    pool.reserve = async () => {
      const reserved = makeReserved(key);
      reservedByKey.set(key, reserved);
      return reserved;
    };
    return pool;
  }

  return {
    log,
    failCommitFor,
    reservedByKey,
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
    count(entry: string): number {
      return log.filter((e) => e === entry).length;
    },
  };
}

let backend: ReturnType<typeof createFakeBackend>;

beforeEach(() => {
  backend = createFakeBackend();
  __setTransactionConnectionFactory(backend.factory);
  setPrimaryDatasource(undefined);
});

afterEach(() => {
  // Restore the real connection factory so this file's fake never leaks into
  // another test file (bun runs test files in one process).
  __setTransactionConnectionFactory(undefined);
  setPrimaryDatasource(undefined);
});

afterAll(() => {
  __setTransactionConnectionFactory(undefined);
});

describe('UnitOfWork — same-datasource atomicity', () => {
  specTest(
    'commits two writes on one datasource together (single BEGIN/COMMIT)',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'lazy-transaction',
      check: 'begin-is-issued-once-per-datasource',
    },
    async () => {
      await runInContext({}, async () => {
        const uow = new UnitOfWork();
        uow.begin();

        const c1 = await useTxConnection('db1', 'write');
        const c2 = await useTxConnection('db1', 'write');
        // Both writes share one reserved connection → one transaction.
        expect(c2).toBe(c1);

        await uow.commit();
      });

      expect(backend.count('db1:begin')).toBe(1);
      expect(backend.count('db1:commit')).toBe(1);
      expect(backend.count('db1:release')).toBe(1);
      expect(backend.count('db1:rollback')).toBe(0);
    },
  );

  it('rolls both writes back on error (no partial commit)', async () => {
    await runInContext({}, async () => {
      const uow = new UnitOfWork();
      uow.begin();

      await useTxConnection('db1', 'write');
      await useTxConnection('db1', 'write');

      await uow.finalize(new Error('boom'));
    });

    expect(backend.count('db1:begin')).toBe(1);
    expect(backend.count('db1:rollback')).toBe(1);
    expect(backend.count('db1:release')).toBe(1);
    expect(backend.count('db1:commit')).toBe(0);
  });
});

describe('UnitOfWork — ambient TX_KEY reconciliation', () => {
  it("a new'd Repository joins the same transaction as the UnitOfWork", async () => {
    const ItemsTable = Table('uow_items', { id: Key(String), name: Column(String) }, { db: 'db1' });

    await runInContext({}, async () => {
      const uow = new UnitOfWork();
      uow.begin();

      // UnitOfWork-driven write reserves the datasource connection...
      const uowConn = await useTxConnection('db1', 'write');

      // ...and a plain `new Repository(...)` resolves the SAME ambient connection.
      const repo = new Repository(ItemsTable);
      const repoConn = await repo.conn('write');
      expect(repoConn).toBe(uowConn);

      await uow.commit();
    });

    // One shared transaction: a single BEGIN + COMMIT, not one per participant.
    expect(backend.count('db1:begin')).toBe(1);
    expect(backend.count('db1:commit')).toBe(1);
    expect(backend.count('db1:release')).toBe(1);
  });

  specTest(
    'commit is idempotent and does not double-finalize',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'single-committer',
      check: 'finalize-happens-at-most-once',
    },
    async () => {
      await runInContext({}, async () => {
        const uow = new UnitOfWork();
        uow.begin();
        await useTxConnection('db1', 'write');

        await uow.commit();
        await uow.commit(); // second call is a no-op
        await uow.rollback(); // after commit, also a no-op

        expect(isTransactionActive()).toBe(false);
      });

      expect(backend.count('db1:commit')).toBe(1);
      expect(backend.count('db1:rollback')).toBe(0);
      expect(backend.count('db1:release')).toBe(1);
    },
  );
});

describe('UnitOfWork — multi-datasource (best-effort, NOT 2PC)', () => {
  it('commits every datasource on success', async () => {
    await runInContext({}, async () => {
      const uow = new UnitOfWork();
      uow.begin();
      await useTxConnection('db1', 'write');
      await useTxConnection('db2', 'write');
      await uow.commit();
    });

    expect(backend.count('db1:commit')).toBe(1);
    expect(backend.count('db2:commit')).toBe(1);
    expect(backend.count('db1:release')).toBe(1);
    expect(backend.count('db2:release')).toBe(1);
  });

  specTest(
    'surfaces a mid-commit failure as an error and releases every connection',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'partial-commit-is-an-error',
      check: 'a-mid-commit-failure-surfaces-and-releases-connections',
    },
    async () => {
      backend.failCommitFor.add('db2');
      let error: Error | undefined;

      await runInContext({}, async () => {
        const uow = new UnitOfWork();
        uow.begin();
        await useTxConnection('db1', 'write');
        await useTxConnection('db2', 'write');
        try {
          await uow.commit();
        } catch (e) {
          error = e as Error;
        }
      });

      // Partial commit is reported as an error, never as success.
      expect(error).toBeDefined();
      // db1 committed before the failure (documented partial commit); db2 failed.
      expect(backend.count('db1:commit')).toBe(1);
      expect(backend.count('db2:commit-fail')).toBe(1);
      // Both connections are released regardless.
      expect(backend.count('db1:release')).toBe(1);
      expect(backend.count('db2:release')).toBe(1);
    },
  );
});

describe('transaction laziness, nesting, timeout (spec)', () => {
  specTest(
    'reserves no connection and issues no BEGIN before the first write',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'lazy-transaction',
      check: 'no-reserve-or-begin-before-the-first-write-and-one-shared-opening',
    },
    async () => {
      await runInContext({}, async () => {
        const uow = new UnitOfWork();
        uow.begin();

        // A read in transactional mode reserves nothing and begins nothing.
        await useTxConnection('db1', 'read');
        expect(backend.reservedByKey.size).toBe(0);
        expect(backend.count('db1:begin')).toBe(0);

        // Two concurrent first writers await the same opening: one BEGIN.
        await Promise.all([useTxConnection('db1', 'write'), useTxConnection('db1', 'write')]);
        expect(backend.count('db1:begin')).toBe(1);

        await uow.commit();
      });

      expect(backend.count('db1:begin')).toBe(1);
      expect(backend.count('db1:commit')).toBe(1);
    },
  );

  specTest(
    'a nested runInTransaction joins the outer unit instead of committing it',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'single-committer',
      check: 'a-nested-run-in-transaction-joins-the-outer-unit',
    },
    async () => {
      await runInContext({}, async () => {
        await runInTransaction(async () => {
          await useTxConnection('db1', 'write');
          await runInTransaction(async () => {
            await useTxConnection('db1', 'write');
          });
          // The inner completion must not have committed the outer unit.
          expect(backend.count('db1:commit')).toBe(0);
        });
      });

      expect(backend.count('db1:begin')).toBe(1);
      expect(backend.count('db1:commit')).toBe(1);
      expect(backend.count('db1:release')).toBe(1);
    },
  );

  specTest(
    'two failed commits aggregate into one AggregateError',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'partial-commit-is-an-error',
      check: 'multi-datasource-commit-failures-aggregate',
    },
    async () => {
      backend.failCommitFor.add('db1');
      backend.failCommitFor.add('db2');
      let error: unknown;

      await runInContext({}, async () => {
        const uow = new UnitOfWork();
        uow.begin();
        await useTxConnection('db1', 'write');
        await useTxConnection('db2', 'write');
        try {
          await uow.commit();
        } catch (e) {
          error = e;
        }
      });

      expect(error).toBeInstanceOf(AggregateError);
      expect((error as AggregateError).errors).toHaveLength(2);
      // Every reserved connection is released even though both commits failed.
      expect(backend.count('db1:release')).toBe(1);
      expect(backend.count('db2:release')).toBe(1);
    },
  );

  specTest(
    'a timed-out transaction rolls back exactly once and a racing commit fails',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'timeout-rollback',
      check: 'timeout-rolls-back-once-and-a-racing-commit-awaits-it-and-fails',
    },
    async () => {
      let error: Error | undefined;
      await runInContext({}, async () => {
        withTransaction({ timeoutMs: 30 });
        await useTxConnection('db1', 'write');

        // Let the timeout fire and start its detached rollback.
        await new Promise((resolve) => setTimeout(resolve, 90));

        try {
          await commit();
        } catch (e) {
          error = e as Error;
        }
      });

      expect(error?.message).toContain('timed out');
      // Exactly one rollback: the timeout's. The racing commit awaited it and
      // failed instead of committing connections the timeout already released.
      expect(backend.count('db1:rollback')).toBe(1);
      expect(backend.count('db1:commit')).toBe(0);
      expect(backend.count('db1:release')).toBe(1);
    },
  );
});

/**
 * Drive the `UnitOfWorkMiddleware` end-to-end against a real DI scope carrying a
 * scoped `UnitOfWork` provider. The single context object doubles as the ALS
 * store (holding both TX_KEY and SCOPE_CONTAINER_KEY) and the HTTP context the
 * middleware reads `signal` from — exactly the shape the HTTP plugin builds.
 */
async function runMiddleware(
  handler: () => Promise<void>,
  opts: { signal?: AbortSignal } = {},
): Promise<{ error?: unknown }> {
  const cc = new ContainerContext('uow-test');
  cc.register(provide(UnitOfWork, () => new UnitOfWork(), { scope: 'scoped' }));
  await cc.start();
  const { scope, close } = cc.createScopeSync();

  // biome-ignore lint/suspicious/noExplicitAny: minimal HTTP-context + ALS-store shape
  const context: any = { signal: opts.signal };
  context[SCOPE_CONTAINER_KEY] = scope;

  const middleware = UnitOfWorkMiddleware();
  let error: unknown;
  try {
    await runInContext(context, async () => {
      await middleware(context, async () => {
        await handler();
        return undefined;
      });
    });
  } catch (e) {
    error = e;
  } finally {
    await close();
    await cc.close();
  }
  return { error };
}

describe('UnitOfWorkMiddleware — boundary', () => {
  it('commits on handler success', async () => {
    const { error } = await runMiddleware(async () => {
      await useTxConnection('db1', 'write');
    });

    expect(error).toBeUndefined();
    expect(backend.count('db1:commit')).toBe(1);
    expect(backend.count('db1:rollback')).toBe(0);
    expect(backend.count('db1:release')).toBe(1);
  });

  it('rolls back and rethrows on handler error', async () => {
    const { error } = await runMiddleware(async () => {
      await useTxConnection('db1', 'write');
      throw new Error('handler blew up');
    });

    expect((error as Error)?.message).toBe('handler blew up');
    expect(backend.count('db1:rollback')).toBe(1);
    expect(backend.count('db1:commit')).toBe(0);
    expect(backend.count('db1:release')).toBe(1);
  });

  it('rolls back and releases connections on AbortSignal', async () => {
    const controller = new AbortController();

    const { error } = await runMiddleware(
      async () => {
        await useTxConnection('db1', 'write');
        // Abort mid-request: the middleware's abort listener rolls back.
        controller.abort();
      },
      { signal: controller.signal },
    );

    expect(error).toBeUndefined();
    expect(backend.count('db1:rollback')).toBe(1);
    expect(backend.count('db1:commit')).toBe(0);
    expect(backend.count('db1:release')).toBe(1);
  });

  it('setRollbackOnly() forces a rollback despite handler success', async () => {
    const { error } = await runMiddleware(async () => {
      await useTxConnection('db1', 'write');
      // The handler resolves the SAME scoped UnitOfWork the middleware drives.
      useContainer().get(UnitOfWork).setRollbackOnly();
    });

    expect(error).toBeUndefined();
    expect(backend.count('db1:rollback')).toBe(1);
    expect(backend.count('db1:commit')).toBe(0);
    expect(backend.count('db1:release')).toBe(1);
  });

  it('leaves the ambient transaction resolved (cleanup safety net is a no-op)', async () => {
    await runMiddleware(async () => {
      await useTxConnection('db1', 'write');
    });

    // A fresh context: nothing left to clean up after the boundary committed.
    await runInContext({}, async () => {
      expect(await cleanupTransaction()).toBe(false);
    });
  });
});
