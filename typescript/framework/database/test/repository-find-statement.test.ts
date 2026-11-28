import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { DateIso, Optional } from '@putnami/runtime';
import { SQL } from 'bun';
import { RepositoryError } from '../src/errors';
import { Column, Key, Repository, Table } from '../src/index';
import { CapturedQuery, createSqlClient } from '../src/postgres/sql-query';
import { MAX_FIND_NODES, MAX_FIND_ORDERS } from '../src/repository/find-statement';
import type { SqlClient } from '../src/sql-client';
import { __setTransactionConnectionFactory } from '../src/transaction';

/**
 * The statement a find sends. A filter that compares each property to a value
 * is one template compiled once per shape, whose strings hold the quoted
 * identifiers and whose values are the filter's, then the limit, then the
 * offset: the statement the composed fragments make, with no fragment. Any
 * other filter is composed as before.
 */

const Items = Table('find_items', {
  id: Key(String),
  ownerId: Column(String),
  isDone: Column(Boolean),
  label: Column(Optional(String)),
  createdAt: Column(DateIso, { columnName: 'created_at' }),
});

const CREATED = new Date('2026-01-02T03:04:05.000Z');

/**
 * Builders come from a real driver that never connects; each query is built
 * on it, recorded, and answers one row. Building runs the driver's own tag,
 * which throws at once on a template it refuses, so a compiled template the
 * driver would not take fails here, without a database.
 */
const builders = new SQL({ hostname: '127.0.0.1', port: 1, database: 'x', username: 'x', max: 1 });
let sent: CapturedQuery[] = [];

function recordingClient(): SqlClient {
  return createSqlClient({
    pool: builders,
    dispatch: (query) => {
      query.build(builders);
      sent.push(query);
      return Promise.resolve(
        Object.assign([{ id: 'i1', ownerId: 'o1', isDone: false, label: null, createdAt: CREATED }], {
          count: 1,
          command: 'SELECT',
        }),
      );
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

/** The strings of the template a query sent: the same object for every query of one cached shape. */
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

beforeEach(() => {
  sent = [];
  const client = Promise.resolve(recordingClient());
  __setTransactionConnectionFactory(() => client);
});

afterEach(() => {
  __setTransactionConnectionFactory(undefined);
});

afterAll(async () => {
  await builders.end({ timeout: 0 });
});

describe('the statement of a find', () => {
  it('sends a point read as one template, with the key then the limit', async () => {
    const found = await new Repository(Items).get({ id: 'i1' });
    expect(found as unknown).toEqual({
      id: 'i1',
      ownerId: 'o1',
      isDone: false,
      label: null,
      createdAt: CREATED.toISOString(),
    });
    expect(sent).toHaveLength(1);
    expect(hasFragment(sent[0])).toBe(false);
    expect(sentTemplate(sent[0])).toEqual({
      strings: ['SELECT * FROM "find_items" WHERE "id" = ', ' LIMIT ', ''],
      values: ['i1', 1],
    });
  });

  it('writes each compared column, the ORDER BY and the OFFSET as the fragments write them', async () => {
    await new Repository(Items).find(
      { ownerId: 'o1', isDone: false },
      { limit: 5, offset: 10, orderBy: 'createdAt desc, id, label DESC' },
    );
    expect(sentTemplate(sent[0])).toEqual({
      strings: [
        'SELECT * FROM "find_items" WHERE "owner_id" = ',
        ' AND "is_done" = ',
        ' ORDER BY "created_at" DESC, "id" ASC, "label" DESC LIMIT ',
        ' OFFSET ',
        '',
      ],
      values: ['o1', false, 5, 10],
    });
  });

  it('reads every row of a table with no filter, under the row ceiling', async () => {
    await new Repository(Items).find();
    expect(sentTemplate(sent[0])).toEqual({ strings: ['SELECT * FROM "find_items" LIMIT ', ''], values: [1000] });
  });

  it('compiles a shape once: the same strings for every repository of the table', async () => {
    await new Repository(Items).get({ id: 'a' });
    await new Repository(Items).get({ id: 'b' });
    await new Repository(Items).find({ id: 'c' }, { limit: 1 });
    const strings = sent.map((query) => (query.template as { strings: TemplateStringsArray }).strings);
    expect(strings[1]).toBe(strings[0] as TemplateStringsArray);
    expect(strings[2]).toBe(strings[0] as TemplateStringsArray);
    expect(sent.map((query) => sentTemplate(query).values)).toEqual([
      ['a', 1],
      ['b', 1],
      ['c', 1],
    ]);
  });

  it('skips an undefined filter value and binds null, a date and an object as values', async () => {
    const repository = new Repository(Items);
    await repository.find({ ownerId: undefined, id: 'i1' } as never, { limit: 1 });
    expect(sentTemplate(sent[0]).strings).toEqual(['SELECT * FROM "find_items" WHERE "id" = ', ' LIMIT ', '']);

    const at = new Date('2026-05-05T00:00:00Z');
    const json = { nested: true };
    await repository.find({ label: null, createdAt: at, ownerId: json } as never);
    expect(sentTemplate(sent[1])).toEqual({
      strings: [
        'SELECT * FROM "find_items" WHERE "label" = ',
        ' AND "created_at" = ',
        ' AND "owner_id" = ',
        ' LIMIT ',
        '',
      ],
      values: [null, at, json, 1000],
    });
  });

  it('composes an operator, an $or and an $and filter from fragments, as before', async () => {
    const repository = new Repository(Items);
    await repository.find({ ownerId: { in: ['o1', 'o2'] } } as never);
    await repository.find({ $or: [{ ownerId: 'o1' }, { isDone: true }] } as never);
    await repository.find({ id: 'i1', $and: [{ isDone: false }] } as never);
    await repository.find({ label: { equals: 'x' } } as never);
    expect(sent).toHaveLength(4);
    for (const query of sent) expect(hasFragment(query)).toBe(true);
  });

  it('refuses an unknown filter property before anything is sent, as the composed path does', async () => {
    const repository = new Repository(Items);
    const compiled = await repository.find({ bogus: 'x' } as never).catch((error: unknown) => error);
    const composed = await repository.find({ bogus: { equals: 'x' } } as never).catch((error: unknown) => error);
    expect(compiled).toBeInstanceOf(RepositoryError);
    expect((compiled as Error).message).toBe((composed as Error).message);
    expect((compiled as RepositoryError).code).toBe((composed as RepositoryError).code);
    expect(sent).toHaveLength(0);
  });

  it('refuses an unknown ORDER BY property with the error of the composed path', async () => {
    const repository = new Repository(Items);
    const compiled = await repository.find({}, { orderBy: 'createdAt, nope DESC' }).catch((error: unknown) => error);
    const composed = await repository
      .find({ label: { equals: 'x' } } as never, { orderBy: 'createdAt, nope DESC' })
      .catch((error: unknown) => error);
    expect(compiled).toBeInstanceOf(RepositoryError);
    expect((compiled as Error).message).toBe(
      'Unknown property "nope" in orderBy. Valid properties: id, ownerId, isDone, label, created_at',
    );
    expect((compiled as Error).message).toBe((composed as Error).message);
    expect(sent).toHaveLength(0);
  });

  it('hands the driver a template object: an own raw that is not enumerable, frozen', async () => {
    await new Repository(Items).get({ id: 'i1' });
    const strings = ((sent[0] as CapturedQuery).template as { strings: TemplateStringsArray }).strings;
    // Bun.SQL takes an array as a template only when its own `raw` is an
    // array and not enumerable; it refuses one whose `raw` is enumerable.
    expect(Object.isFrozen(strings)).toBe(true);
    expect(Object.hasOwn(strings, 'raw')).toBe(true);
    expect(Object.getOwnPropertyDescriptor(strings, 'raw')?.enumerable).toBe(false);
    expect([...strings.raw]).toEqual([...strings]);
  });

  it('keeps an empty ORDER BY option out of the statement', async () => {
    await new Repository(Items).find({ id: 'i1' }, { orderBy: ' , ' });
    expect(sentTemplate(sent[0]).strings).toEqual(['SELECT * FROM "find_items" WHERE "id" = ', ' LIMIT ', '']);
  });

  it('keys an ORDER BY on its clause: its spellings share one entry and leave a new filter shape its room', async () => {
    const Spelled = Table('find_spelled', { id: Key(String), ownerId: Column(String) });
    const repository = new Repository(Spelled);
    // More spellings of `"id" ASC` than the table holds entries, as a request can send them.
    const spellings = Array.from(
      { length: MAX_FIND_NODES + MAX_FIND_ORDERS },
      (_, i) => `${' '.repeat(i)}id${['', ' asc', ' ASC', ',', ' anything'][i % 5]}`,
    );
    for (const orderBy of spellings) await repository.find({ id: 'x' }, { orderBy });
    const ordered = sent.map(templateOf);
    expect(new Set(ordered).size).toBe(1);
    expect([...(ordered[0] as TemplateStringsArray)]).toEqual([
      'SELECT * FROM "find_spelled" WHERE "id" = ',
      ' ORDER BY "id" ASC LIMIT ',
      '',
    ]);

    sent = [];
    await repository.find({ ownerId: 'o1' });
    await repository.find({ ownerId: 'o2' });
    expect(templateOf(sent[1])).toBe(templateOf(sent[0]));
  });

  it('holds ORDER BY clauses under their own bound, apart from the filter shapes', async () => {
    const Ordered = Table('find_ordered', { id: Key(String), ownerId: Column(String) });
    const repository = new Repository(Ordered);
    // More distinct clauses than the table holds: `"id" ASC`, `"id" ASC, "id" ASC`, …
    const clauses = Array.from({ length: MAX_FIND_ORDERS + 2 }, (_, i) =>
      Array(i + 1)
        .fill('id')
        .join(','),
    );
    for (const orderBy of clauses) await repository.find({ id: 'x' }, { orderBy });
    expect(new Set(sent.map(templateOf)).size).toBe(clauses.length);

    // Past the bound, a new clause is compiled for its call, and still sends its statement.
    sent = [];
    await repository.find({ id: 'x' }, { orderBy: 'ownerId desc' });
    await repository.find({ id: 'y' }, { orderBy: 'ownerId DESC' });
    expect(templateOf(sent[1])).not.toBe(templateOf(sent[0]));
    expect(sentTemplate(sent[1])).toEqual({
      strings: ['SELECT * FROM "find_ordered" WHERE "id" = ', ' ORDER BY "owner_id" DESC LIMIT ', ''],
      values: ['y', 1000],
    });

    // A filter shape is still held: the ORDER BY entries took none of its room.
    sent = [];
    await repository.find({ ownerId: 'o1' });
    await repository.find({ ownerId: 'o2' });
    expect(templateOf(sent[1])).toBe(templateOf(sent[0]));
  });

  it('hands an overriding find options it may change', async () => {
    type Find = Repository<typeof Items>['find'];
    class Defaulted extends Repository<typeof Items> {
      override find(filters?: Parameters<Find>[0], options: NonNullable<Parameters<Find>[1]> = {}) {
        options.orderBy ??= 'createdAt desc';
        return super.find(filters, options);
      }
    }
    const found = await new Defaulted(Items).findOne({ id: 'i1' });
    expect(found?.id).toBe('i1');
    expect(sentTemplate(sent[0])).toEqual({
      strings: ['SELECT * FROM "find_items" WHERE "id" = ', ' ORDER BY "created_at" DESC LIMIT ', ''],
      values: ['i1', 1],
    });
  });
});
