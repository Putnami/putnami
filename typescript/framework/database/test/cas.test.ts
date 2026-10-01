import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import {
  Column,
  Key,
  Outcome,
  Repository,
  RepositoryError,
  Table,
  classifyOutcome,
  outcomeRetryable,
  runInTransaction,
} from '../src/index';
import { setPrimaryDatasource } from '../src/primary-datasource';
import { __setTransactionConnectionFactory } from '../src/transaction';

const DB_NAME = 'cas-test';

// Columns are declared but never read by the CAS helpers — those build raw SQL
// fragments (SET / guard / WHERE) and only need the table name — so a minimal
// definition is enough to exercise the outcome logic.
const TokensTable = Table(
  'tokens',
  {
    id: Key(String),
    state: Column(String),
  },
  { db: DB_NAME },
);

class TokenRepository extends Repository<typeof TokensTable> {
  constructor() {
    super(TokensTable);
  }
}

/** A pg-shaped driver error carrying a SQLSTATE on `code`, like postgres.js throws. */
function pgError(code: string): Error {
  return Object.assign(new Error(`pg error ${code}`), { code });
}

/** A postgres.js RowList stand-in: an array carrying the affected-row `count`. */
function rowsWithCount(count: number, data: Record<string, unknown>[] = []): Record<string, unknown>[] {
  const rows = [...data] as Record<string, unknown>[] & { count: number };
  rows.count = count;
  return rows;
}

/**
 * A deterministic, in-memory stand-in for the postgres.js connection. Its
 * `unsafe(query, args)` routes to a scripted handler keyed on the leading verb of
 * the SQL, so the CAS outcome logic is pinned without a live Postgres. The same
 * object serves as both the pool (`reserve()`) and the reserved connection
 * (callable for BEGIN/COMMIT/ROLLBACK, `release()`), so a helper run inside a
 * transaction resolves this single connection for every statement.
 */
interface Script {
  update?: number | Error;
  exists?: boolean | Error;
  insert?: 'ok' | Error;
}

function createFakeSql(script: Script) {
  const calls: { query: string; args: readonly unknown[] }[] = [];

  const handler = (query: string): Record<string, unknown>[] => {
    if (query.startsWith('UPDATE')) {
      if (script.update instanceof Error) throw script.update;
      return rowsWithCount(script.update ?? 0);
    }
    if (query.includes('SELECT EXISTS')) {
      if (script.exists instanceof Error) throw script.exists;
      return [{ present: script.exists === true }];
    }
    if (query.startsWith('INSERT')) {
      if (script.insert instanceof Error) throw script.insert;
      return rowsWithCount(1);
    }
    return rowsWithCount(0);
  };

  // biome-ignore lint/suspicious/noExplicitAny: minimal postgres.Sql-shaped fake
  const sql = ((_strings: TemplateStringsArray) => Promise.resolve([])) as any;
  sql.unsafe = (query: string, args: readonly unknown[] = []) => {
    calls.push({ query, args });
    try {
      return Promise.resolve(handler(query));
    } catch (error) {
      return Promise.reject(error);
    }
  };
  sql.reserve = async () => sql;
  sql.release = () => {};

  return { sql, calls };
}

function installFake(script: Script) {
  const fake = createFakeSql(script);
  __setTransactionConnectionFactory(async () => fake.sql);
  return fake;
}

let repo: TokenRepository;

beforeEach(() => {
  setPrimaryDatasource(undefined);
  repo = new TokenRepository();
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

describe('Outcome enum — cross-language byte parity', () => {
  // These string values MUST match the Go enum (transaction.Outcome*) and the
  // schema at protocols/transaction/schemas/transaction-result.json byte-for-byte.
  it('mirrors the four Go Outcome literals exactly', () => {
    expect(Outcome.Applied).toBe('applied');
    expect(Outcome.AlreadyConsumedConflict).toBe('already-consumed-conflict');
    expect(Outcome.NotFound).toBe('not-found');
    expect(Outcome.RetryableSerializationFailure).toBe('retryable-serialization-failure');
  });

  it('exposes exactly the closed set of outcome values', () => {
    expect([...Object.values(Outcome)].sort()).toEqual(
      ['already-consumed-conflict', 'applied', 'not-found', 'retryable-serialization-failure'].sort(),
    );
  });

  it('marks only the serialization failure retryable', () => {
    expect(outcomeRetryable(Outcome.RetryableSerializationFailure)).toBe(true);
    expect(outcomeRetryable(Outcome.Applied)).toBe(false);
    expect(outcomeRetryable(Outcome.AlreadyConsumedConflict)).toBe(false);
    expect(outcomeRetryable(Outcome.NotFound)).toBe(false);
  });
});

describe('classifyOutcome — SQLSTATE → Outcome', () => {
  it('maps 40001 and 40P01 to retryable-serialization-failure', () => {
    expect(classifyOutcome(pgError('40001'))).toBe(Outcome.RetryableSerializationFailure);
    expect(classifyOutcome(pgError('40P01'))).toBe(Outcome.RetryableSerializationFailure);
  });

  it('maps 23505 to already-consumed-conflict', () => {
    expect(classifyOutcome(pgError('23505'))).toBe(Outcome.AlreadyConsumedConflict);
  });

  it('passes an unrelated SQLSTATE through as undefined (raw error)', () => {
    expect(classifyOutcome(pgError('08006'))).toBeUndefined();
    expect(classifyOutcome(pgError('23503'))).toBeUndefined();
  });

  it('returns undefined for a non-postgres error and for nullish input', () => {
    expect(classifyOutcome(new Error('boom'))).toBeUndefined();
    expect(classifyOutcome(undefined)).toBeUndefined();
    expect(classifyOutcome(null)).toBeUndefined();
  });
});

describe('compareAndSet', () => {
  it('returns applied when the row matched and transitioned', async () => {
    installFake({ update: 1 });
    expect(await repo.compareAndSet('id', 't1', 'state', 'active', 'revoked')).toBe(Outcome.Applied);
  });

  it('returns already-consumed-conflict when the row exists but value differs', async () => {
    installFake({ update: 0, exists: true });
    expect(await repo.compareAndSet('id', 't1', 'state', 'active', 'revoked')).toBe(Outcome.AlreadyConsumedConflict);
  });

  it('returns not-found when no row has the key', async () => {
    installFake({ update: 0, exists: false });
    expect(await repo.compareAndSet('id', 'missing', 'state', 'active', 'revoked')).toBe(Outcome.NotFound);
  });

  it('classifies a serialization failure on the UPDATE as retryable', async () => {
    installFake({ update: pgError('40001') });
    expect(await repo.compareAndSet('id', 't1', 'state', 'active', 'revoked')).toBe(
      Outcome.RetryableSerializationFailure,
    );
  });

  it('binds SET/key/expected in the mirrored $1/$2/$3 order', async () => {
    const fake = installFake({ update: 1 });
    await repo.compareAndSet('id', 't1', 'state', 'active', 'revoked');
    const update = fake.calls.find((c) => c.query.startsWith('UPDATE'));
    expect(update?.query).toBe('UPDATE "tokens" SET "state" = $1 WHERE "id" = $2 AND "state" = $3');
    expect(update?.args).toEqual(['revoked', 't1', 'active']);
  });

  it('throws on an invalid identifier rather than emitting it', async () => {
    installFake({ update: 1 });
    await expect(repo.compareAndSet('id"; DROP', 't1', 'state', 'a', 'b')).rejects.toThrow(/identifier/);
  });
});

describe('consumeOnce', () => {
  it('returns applied when the guard held and the row was claimed', async () => {
    installFake({ update: 1 });
    expect(await repo.consumeOnce('id', 't1', 'consumed = false', 'consumed = true')).toBe(Outcome.Applied);
  });

  it('returns already-consumed-conflict when the row exists but the guard no longer holds', async () => {
    installFake({ update: 0, exists: true });
    expect(await repo.consumeOnce('id', 't1', 'consumed = false', 'consumed = true')).toBe(
      Outcome.AlreadyConsumedConflict,
    );
  });

  it('returns not-found when no row has the key', async () => {
    installFake({ update: 0, exists: false });
    expect(await repo.consumeOnce('id', 'missing', 'consumed = false', 'consumed = true')).toBe(Outcome.NotFound);
  });

  it('binds the key as the final positional parameter after the caller args', async () => {
    const fake = installFake({ update: 1 });
    await repo.consumeOnce('id', 't1', 'consumed = false', 'consumed = true, consumed_by = $1', 'user-9');
    const update = fake.calls.find((c) => c.query.startsWith('UPDATE'));
    // set/guard args first ($1 = 'user-9'), key appended last ($2).
    expect(update?.query).toBe(
      'UPDATE "tokens" SET consumed = true, consumed_by = $1 WHERE (consumed = false) AND "id" = $2',
    );
    expect(update?.args).toEqual(['user-9', 't1']);
  });
});

describe('rotateRow', () => {
  const spec = () => ({
    keyColumn: 'id',
    predecessorKey: 'old',
    stateColumn: 'state',
    expected: 'active',
    revoked: 'revoked',
    successorColumns: ['id', 'state'],
    successorValues: ['new', 'active'],
  });

  it('returns applied when the predecessor was revoked and the successor inserted', async () => {
    const fake = installFake({ update: 1, insert: 'ok' });
    expect(await repo.rotateRow(spec())).toBe(Outcome.Applied);
    // Revoke (UPDATE) then install (INSERT) — both writes on the same connection.
    expect(fake.calls.map((c) => c.query.split(' ')[0])).toEqual(['UPDATE', 'INSERT']);
  });

  it('skips the successor insert when the predecessor could not be revoked', async () => {
    const fake = installFake({ update: 0, exists: true });
    expect(await repo.rotateRow(spec())).toBe(Outcome.AlreadyConsumedConflict);
    expect(fake.calls.some((c) => c.query.startsWith('INSERT'))).toBe(false);
  });

  it('returns not-found (no insert) when the predecessor is absent', async () => {
    const fake = installFake({ update: 0, exists: false });
    expect(await repo.rotateRow(spec())).toBe(Outcome.NotFound);
    expect(fake.calls.some((c) => c.query.startsWith('INSERT'))).toBe(false);
  });

  it('maps a unique violation on the successor insert to a conflict', async () => {
    installFake({ update: 1, insert: pgError('23505') });
    expect(await repo.rotateRow(spec())).toBe(Outcome.AlreadyConsumedConflict);
  });

  it('rejects a mismatched successor columns/values length', async () => {
    installFake({ update: 1 });
    await expect(
      repo.rotateRow({ ...spec(), successorColumns: ['id'], successorValues: ['new', 'active'] }),
    ).rejects.toThrow(/length mismatch/);
  });

  it('runs inside a transaction, joining the ambient connection for every write', async () => {
    const fake = installFake({ update: 1, insert: 'ok' });
    const outcome = await runInContext({}, () => runInTransaction(async () => repo.rotateRow(spec())));
    expect(outcome).toBe(Outcome.Applied);
    // The revoke UPDATE, the INSERT, all resolved the one reserved connection.
    expect(fake.calls.map((c) => c.query.split(' ')[0])).toEqual(['UPDATE', 'INSERT']);
  });

  // A primitive named `rotate` would collide with subclasses' own domain
  // `rotate` methods, so it is named `rotateRow`. The compile-time gate lives in
  // src/repository/subclass-compat.check.ts; this pins the runtime behavior —
  // the domain override and the inherited primitive coexist.
  it('leaves the rotate name free for a subclass domain method', async () => {
    class ApiKeyRepository extends TokenRepository {
      async rotate(id: string, _prefix: string, _tokenHash: string): Promise<Outcome> {
        return this.rotateRow({ ...spec(), predecessorKey: id });
      }
    }
    installFake({ update: 1, insert: 'ok' });
    const apiKeys = new ApiKeyRepository();
    expect(await apiKeys.rotate('old', 'pk_live', 'hash')).toBe(Outcome.Applied);
    expect(await apiKeys.rotateRow(spec())).toBe(Outcome.Applied);
  });
});

describe('updateWhere', () => {
  it('returns the affected-row count', async () => {
    installFake({ update: 3 });
    expect(await repo.updateWhere('state = $1', 'state = $2', 'revoked', 'active')).toBe(3);
  });

  it('wraps a driver failure as a RepositoryError', async () => {
    installFake({ update: pgError('08006') });
    await expect(repo.updateWhere('state = $1', 'state = $2', 'revoked', 'active')).rejects.toBeInstanceOf(
      RepositoryError,
    );
  });
});
