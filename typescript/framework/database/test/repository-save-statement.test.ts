import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { drainMetrics } from '@putnami/application/testing';
import { DateIso, Int, Optional, resetDefaultLogger, runInContext, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { SQL } from 'bun';
import { QueryAbortError } from '../src/abort';
import { RepositoryError } from '../src/errors';
import { Column, entityHelper, Key, Repository, Table } from '../src/index';
import { CapturedQuery, createSqlClient } from '../src/postgres/sql-query';
import { MAX_SAVE_NODES } from '../src/repository/save-statement';
import type { SqlClient } from '../src/sql-client';
import { __setTransactionConnectionFactory } from '../src/transaction';

/**
 * The statement a save sends. Each sequence of properties an entity holds is
 * one template compiled once, whose strings hold the quoted table, columns,
 * conflict target and assignments, and whose values are the entity's database
 * values in order: the statement the composed fragments make, with no
 * fragment. A value that is itself a query of the client is composed as
 * before. What a save checks, throws, records and returns does not change.
 */

const Items = Table('save_items', {
  id: Key(String),
  ownerId: Column(String),
  isDone: Column(Boolean),
  label: Column(Optional(String)),
  createdAt: Column(DateIso, { columnName: 'created_at', default: 'NOW()' }),
});

const CREATED = '2026-01-02T03:04:05.000Z';

/**
 * Builders come from a real driver that never connects; each query is built
 * on it, recorded, and answers the row `answer` returns. Building runs the
 * driver's own tag, which throws at once on a template it refuses.
 */
const builders = new SQL({ hostname: '127.0.0.1', port: 1, database: 'x', username: 'x', max: 1 });
let sent: CapturedQuery[] = [];
let answer: () => unknown[] = () => [
  { id: 'i1', ownerId: 'o1', isDone: false, label: null, createdAt: new Date(CREATED) },
];

function recordingClient(): SqlClient {
  return createSqlClient({
    pool: builders,
    dispatch: (query) => {
      query.build(builders);
      sent.push(query);
      return Promise.resolve(Object.assign(answer(), { count: 1, command: 'INSERT' }));
    },
    reserve: () => Promise.reject(new Error('not reserved here')),
    begin: () => Promise.reject(new Error('no transaction here')),
    end: async () => {},
  });
}

/** The strings and values of the template a query sent. */
function sentTemplate(query: CapturedQuery | undefined): { strings: readonly string[]; values: readonly unknown[] } {
  const template = query?.template;
  if (template?.kind !== 'template') throw new Error(`not a template: ${template?.kind}`);
  return { strings: [...template.strings], values: template.values };
}

function templateOf(query: CapturedQuery | undefined): TemplateStringsArray {
  const template = query?.template;
  if (template?.kind !== 'template') throw new Error(`not a template: ${template?.kind}`);
  return template.strings;
}

/** Whether a template holds another captured query: a fragment the driver replays. */
function hasFragment(query: CapturedQuery | undefined): boolean {
  const template = query?.template;
  return template?.kind === 'template' && template.values.some((value) => value instanceof CapturedQuery);
}

async function failure(pending: Promise<unknown>): Promise<Error> {
  try {
    await pending;
  } catch (error) {
    return error as Error;
  }
  throw new Error('expected a rejection');
}

beforeEach(() => {
  sent = [];
  answer = () => [{ id: 'i1', ownerId: 'o1', isDone: false, label: null, createdAt: new Date(CREATED) }];
  const client = Promise.resolve(recordingClient());
  __setTransactionConnectionFactory(() => client);
});

afterEach(() => {
  __setTransactionConnectionFactory(undefined);
  setCollector(undefined);
  resetDefaultLogger();
});

afterAll(async () => {
  await builders.end({ timeout: 0 });
});

describe('the statement of a save', () => {
  it('sends a save as one template: the columns in the entity order, the conflict target, the assignments', async () => {
    const saved = await new Repository(Items).save({
      id: 'i1',
      ownerId: 'o1',
      isDone: false,
      label: undefined,
      createdAt: CREATED,
    });
    expect(saved as unknown).toEqual({ id: 'i1', ownerId: 'o1', isDone: false, label: null, createdAt: CREATED });
    expect(sent).toHaveLength(1);
    expect(hasFragment(sent[0])).toBe(false);
    expect(sentTemplate(sent[0])).toEqual({
      strings: [
        'INSERT INTO "save_items" ("id", "owner_id", "is_done", "label", "created_at") VALUES (',
        ', ',
        ', ',
        ', ',
        ', ',
        ') ON CONFLICT ("id") DO UPDATE SET "owner_id" = EXCLUDED."owner_id", "is_done" = EXCLUDED."is_done", "label" = EXCLUDED."label", "created_at" = EXCLUDED."created_at" RETURNING *',
      ],
      // An undefined value is sent as NULL, as the row builder sends it.
      values: ['i1', 'o1', false, null, CREATED],
    });
  });

  it('compiles a shape once: the same strings for every repository of the table, others for another order', async () => {
    await new Repository(Items).save({ id: 'a', ownerId: 'o1' });
    await new Repository(Items).save({ id: 'b', ownerId: 'o2' });
    await new Repository(Items).save({ ownerId: 'o3', id: 'c' });
    expect(templateOf(sent[1])).toBe(templateOf(sent[0]));
    expect(templateOf(sent[2])).not.toBe(templateOf(sent[0]));
    expect(sentTemplate(sent[2])).toEqual({
      strings: [
        'INSERT INTO "save_items" ("owner_id", "id") VALUES (',
        ', ',
        ') ON CONFLICT ("id") DO UPDATE SET "owner_id" = EXCLUDED."owner_id" RETURNING *',
      ],
      values: ['o3', 'c'],
    });
    expect(sent.map((query) => sentTemplate(query).values)).toEqual([
      ['a', 'o1'],
      ['b', 'o2'],
      ['o3', 'c'],
    ]);
  });

  it('assigns the first key to itself when the entity holds only keys, so the row still comes back', async () => {
    await new Repository(Items).save({ id: 'i1' });
    expect(sentTemplate(sent[0])).toEqual({
      strings: [
        'INSERT INTO "save_items" ("id") VALUES (',
        ') ON CONFLICT ("id") DO UPDATE SET "id" = "save_items"."id" RETURNING *',
      ],
      values: ['i1'],
    });
  });

  it('names every key column of the table as the conflict target, in schema order', async () => {
    const Pairs = Table('save_pairs', {
      tenantId: Key(String),
      label: Column(String),
      slug: Key(String, { columnName: 'slug_key' }),
    });
    answer = () => [{ tenantId: 't', slug_key: 's', label: 'x' }];
    const saved = await new Repository(Pairs).save({ label: 'x', slug: 's', tenantId: 't' });
    expect(saved as unknown).toEqual({ tenantId: 't', slug: 's', label: 'x' });
    expect(sentTemplate(sent[0])).toEqual({
      strings: [
        'INSERT INTO "save_pairs" ("label", "slug_key", "tenant_id") VALUES (',
        ', ',
        ', ',
        ') ON CONFLICT ("tenant_id", "slug_key") DO UPDATE SET "label" = EXCLUDED."label" RETURNING *',
      ],
      values: ['x', 's', 't'],
    });

    // One key is enough to save, the conflict target still names both.
    await new Repository(Pairs).save({ tenantId: 't', label: 'y' });
    expect(sentTemplate(sent[1]).strings.at(-1)).toBe(
      ') ON CONFLICT ("tenant_id", "slug_key") DO UPDATE SET "label" = EXCLUDED."label" RETURNING *',
    );
  });

  it('converts each value once, in order, with its column transform', async () => {
    const calls: unknown[] = [];
    const Coded = Table('save_coded', {
      id: Key(String),
      code: Column(String, {
        toDatabase: (value) => {
          calls.push(value);
          return `db:${String(value)}`;
        },
      }),
      rank: Column(Int),
    });
    answer = () => [{ id: 'k', code: 'db:c', rank: 2 }];
    await new Repository(Coded).save({ rank: 2, code: 'c', id: 'k' });
    expect(calls).toEqual(['c']);
    expect(sentTemplate(sent[0]).values).toEqual([2, 'db:c', 'k']);
  });

  it('sends a value that is a query of the client in a row builder, composed as before, never as SQL', async () => {
    const Coded = Table('save_queried', {
      id: Key(String),
      note: Column(String, { toDatabase: () => recordingClient()`now()` as never }),
    });
    await new Repository(Coded).save({ id: 'k', note: 'x' });
    expect(sent).toHaveLength(1);
    // The fragments of the composed statement, the query only as a cell of the row builder.
    expect(hasFragment(sent[0])).toBe(true);
    const { values } = sentTemplate(sent[0]);
    const rowBuilder = values[1] as { value: Record<string, unknown>[]; columns: string[] };
    expect(rowBuilder.columns).toEqual(['id', 'note']);
    expect(rowBuilder.value[0]?.['note']).toBeInstanceOf(CapturedQuery);
  });

  it('throws the errors of the general path, in its order, and sends nothing', async () => {
    const repository = new Repository(Items);

    const empty = await failure(repository.save({}));
    expect(empty).toBeInstanceOf(RepositoryError);
    expect((empty as RepositoryError).code).toBe('EMPTY_ENTITY');

    const keyless = await failure(repository.save({ ownerId: 'o1', id: null } as never));
    expect((keyless as RepositoryError).code).toBe('MISSING_PRIMARY_KEY');
    expect(keyless.message).toBe('Cannot save an entity without primary key');

    // Validation comes before the unknown property, which only the statement refuses.
    const invalid = await failure(repository.save({ id: 'i1', isDone: 'yes', bogus: 1 } as never));
    expect((invalid as RepositoryError).code).toBe('VALIDATION_ERROR');
    expect(invalid.message).toBe('Validation failed: save_items.isDone: save_items.isDone must be of type boolean');

    const unknown = await failure(repository.save({ id: 'i1', bogus: 1 } as never));
    let expected: unknown;
    try {
      entityHelper(Items).columnName('bogus');
    } catch (error) {
      expected = error;
    }
    expect(unknown).toBeInstanceOf(RepositoryError);
    expect(unknown.message).toBe((expected as Error).message);
    expect((unknown as RepositoryError).code).toBe((expected as RepositoryError).code);
    expect(sent).toHaveLength(0);
  });

  it('validates a strict save against the full schema, as before', async () => {
    const repository = new Repository(Items);
    const missing = await failure(repository.save({ id: 'i1', ownerId: 'o1' }, { strict: true }));
    expect(missing.message).toBe('Validation failed: save_items.isDone: save_items.isDone is required');
    await repository.save({ id: 'i1', ownerId: 'o1', isDone: true }, { strict: true });
    expect(sentTemplate(sent[0]).values).toEqual(['i1', 'o1', true]);
  });

  it('throws an aborted request before the statement, after the validation', async () => {
    const controller = new AbortController();
    controller.abort();
    const repository = new Repository(Items);
    const invalid = await failure(
      runInContext({ signal: controller.signal }, () =>
        repository.save({ id: 'i1', isDone: 'no' } as never),
      ) as Promise<unknown>,
    );
    expect((invalid as RepositoryError).code).toBe('VALIDATION_ERROR');
    const aborted = await failure(
      runInContext({ signal: controller.signal }, () =>
        repository.save({ id: 'i1', isDone: true }),
      ) as Promise<unknown>,
    );
    expect(aborted).toBeInstanceOf(QueryAbortError);
    expect(sent).toHaveLength(0);
  });

  it('wraps a failed statement and a missing row as before', async () => {
    answer = () => [];
    const none = await failure(new Repository(Items).save({ id: 'i1' }));
    expect(none).toBeInstanceOf(RepositoryError);
    expect(none.message).toBe('Failed to save entity');
    expect((none as RepositoryError).code).toBe('SAVE_ERROR');
    expect(((none as RepositoryError).cause as RepositoryError).code).toBe('SAVE_FAILED');

    answer = () => {
      throw Object.assign(new Error('duplicate key value'), { code: '23505' });
    };
    const duplicate = await failure(new Repository(Items).save({ id: 'i1' }));
    expect(duplicate.message).toBe('Failed to save entity');
    expect((duplicate as RepositoryError).code).toBe('UNIQUE_VIOLATION');
  });

  it('records the metrics and the log record of a save, and of a failed one', async () => {
    const collector = new TelemetryCollector();
    setCollector(collector);
    const logger = new MemoryLogger();
    setRootLogger(logger);

    await new Repository(Items).save({ id: 'i1' });
    await failure(new Repository(Items).save({ id: 'i1', bogus: 1 } as never));

    const bucket = drainMetrics(collector);
    expect(bucket.counters['sql.save.save_items']).toBe(1);
    expect(bucket.histograms['sql.save.save_items.duration']?.count).toBe(1);
    expect(bucket.counters['sql.save.save_items.error']).toBe(1);
    expect(bucket.counters['sql.query.error']).toBe(1);
    expect(bucket.histograms['sql.query.duration']?.count).toBe(2);

    const groups = logger.entries.map((entry) => ({
      message: entry.message,
      database: (entry.data?.[0] as Record<string, Record<string, unknown>> | undefined)?.['database'],
    }));
    expect(groups.map((group) => group.message)).toEqual(['query executed', 'query failed']);
    expect(groups[0]?.database).toMatchObject({ operation: 'save', table: 'save_items', rowCount: 1 });
    expect(groups[1]?.database).toMatchObject({ operation: 'save', table: 'save_items' });
  });

  it('holds shapes under their own bound, apart from the find shapes of the table', async () => {
    const Wide = Table('save_wide', {
      id: Key(String),
      a: Column(String),
      b: Column(String),
      c: Column(String),
      d: Column(String),
    });
    answer = () => [{ id: 'x', a: 'a', b: 'b', c: 'c', d: 'd' }];
    const repository = new Repository(Wide);
    // Every order of the five properties: more nodes than a table holds.
    const permutations = (items: string[]): string[][] =>
      items.length <= 1
        ? [items]
        : items.flatMap((item, i) =>
            permutations([...items.slice(0, i), ...items.slice(i + 1)]).map((rest) => [item, ...rest]),
          );
    // 120 orders, 325 nodes: 5 first properties, 20 pairs, 60 triples, 120 and 120.
    const orders = permutations(['id', 'a', 'b', 'c', 'd']);
    expect(MAX_SAVE_NODES).toBeLessThan(325);
    for (const order of orders) await repository.save(Object.fromEntries(order.map((prop) => [prop, prop])) as never);
    expect(sent).toHaveLength(orders.length);

    // Past the bound, a new shape is compiled for its call, and still sends its statement.
    sent = [];
    const last = Object.fromEntries((orders.at(-1) as string[]).map((prop) => [prop, prop])) as never;
    await repository.save(last);
    await repository.save(last);
    expect(templateOf(sent[1])).not.toBe(templateOf(sent[0]));
    expect(sentTemplate(sent[1])).toEqual({
      strings: [
        'INSERT INTO "save_wide" ("d", "c", "b", "a", "id") VALUES (',
        ', ',
        ', ',
        ', ',
        ', ',
        ') ON CONFLICT ("id") DO UPDATE SET "d" = EXCLUDED."d", "c" = EXCLUDED."c", "b" = EXCLUDED."b", "a" = EXCLUDED."a" RETURNING *',
      ],
      values: ['d', 'c', 'b', 'a', 'id'],
    });

    // A shape held before the bound is still held, and a find shape has its own room.
    sent = [];
    await repository.save({ id: 'id', a: 'a', b: 'b', c: 'c', d: 'd' });
    await repository.save({ id: 'id', a: 'a', b: 'b', c: 'c', d: 'd' });
    expect(templateOf(sent[1])).toBe(templateOf(sent[0]));
    sent = [];
    await repository.find({ a: 'a' });
    await repository.find({ a: 'b' });
    expect(templateOf(sent[1])).toBe(templateOf(sent[0]));
  });

  it('hands the driver a template object: an own raw that is not enumerable, frozen', async () => {
    await new Repository(Items).save({ id: 'i1', ownerId: 'o1' });
    const strings = templateOf(sent[0]);
    expect(Object.isFrozen(strings)).toBe(true);
    expect(Object.getOwnPropertyDescriptor(strings, 'raw')?.enumerable).toBe(false);
    expect([...strings.raw]).toEqual([...strings]);
  });
});
