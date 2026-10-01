import { describe, expect, it } from 'bun:test';
import type postgres from 'postgres';
import { Column, Key, Table } from '../src/table';
import { Repository } from '../src/repository/repository';

/**
 * DB-free stand-in for the postgres.js tagged template + `sql(value)` helper.
 * Records emitted fragments so tests can assert the generated SQL shape, that
 * identifiers are quoted (escaped) rather than concatenated raw, and that any
 * nested fragment is spliced in. See repository-where.test.ts for the same
 * shape; duplicated here to keep each suite self-contained and DB-free.
 */
interface Fragment {
  readonly kind: 'fragment';
  readonly sql: string;
  readonly params: unknown[];
}
interface Ident {
  readonly kind: 'ident';
  readonly name: string;
}
interface ValueList {
  readonly kind: 'valuelist';
  readonly values: readonly unknown[];
}
type Node = Fragment | Ident | ValueList;

function isNode(value: unknown): value is Node {
  return (
    typeof value === 'object' &&
    value !== null &&
    'kind' in value &&
    ((value as Node).kind === 'fragment' || (value as Node).kind === 'ident' || (value as Node).kind === 'valuelist')
  );
}

function makeSqlStub(): postgres.Sql {
  const tag = (strings: TemplateStringsArray | string, ...values: unknown[]): unknown => {
    if (!Array.isArray(strings) || !('raw' in (strings as TemplateStringsArray))) {
      if (Array.isArray(strings)) {
        return { kind: 'valuelist', values: strings } satisfies ValueList;
      }
      return { kind: 'ident', name: strings as string } satisfies Ident;
    }
    const tpl = strings as TemplateStringsArray;
    let sql = '';
    const params: unknown[] = [];
    for (let i = 0; i < tpl.length; i++) {
      sql += tpl[i];
      if (i < values.length) {
        const v = values[i];
        if (isNode(v)) {
          if (v.kind === 'ident') {
            sql += `"${v.name}"`;
          } else if (v.kind === 'valuelist') {
            sql += `(${v.values.map(() => '?').join(', ')})`;
            params.push(...v.values);
          } else {
            sql += v.sql;
            params.push(...v.params);
          }
        } else {
          sql += '?';
          params.push(v);
        }
      }
    }
    return { kind: 'fragment', sql, params } satisfies Fragment;
  };
  return tag as unknown as postgres.Sql;
}

function frag(value: postgres.PendingQuery<postgres.Row[]>): Fragment {
  return value as unknown as Fragment;
}

const OrderTable = Table('orders', {
  id: Key(String),
  name: Column(String),
  createdAt: Column(String, { columnName: 'created_at' }),
});

/**
 * Test seam exposing the private SQL-builder methods. They are pure given a
 * `sql` tag (no connection), so a DB-free stub is enough to assert their
 * identifier-escaping and ordering behaviour — the injection surface.
 */
class OrderRepo extends Repository<typeof OrderTable> {
  constructor() {
    super(OrderTable);
  }
  orderBy(sql: postgres.Sql, spec?: string): postgres.PendingQuery<postgres.Row[]> {
    return (this as unknown as { buildOrderBy: typeof OrderRepo.prototype.orderBy }).buildOrderBy(sql, spec);
  }
  idents(sql: postgres.Sql, names: string[]): postgres.PendingQuery<postgres.Row[]> {
    return (this as unknown as { identList: typeof OrderRepo.prototype.idents }).identList(sql, names);
  }
}

describe('Repository.buildOrderBy', () => {
  const repo = new OrderRepo();

  it('returns an empty fragment when no orderBy is given', () => {
    const sql = makeSqlStub();
    expect(frag(repo.orderBy(sql, undefined)).sql).toBe('');
    expect(frag(repo.orderBy(sql, '')).sql).toBe('');
  });

  it('emits ORDER BY with the resolved column and an explicit default ASC', () => {
    const sql = makeSqlStub();
    const f = frag(repo.orderBy(sql, 'name'));
    expect(f.sql).toBe('ORDER BY "name" ASC');
    expect(f.params).toEqual([]);
  });

  it('maps a property to its custom column name', () => {
    const sql = makeSqlStub();
    expect(frag(repo.orderBy(sql, 'createdAt DESC')).sql).toBe('ORDER BY "created_at" DESC');
  });

  it('honors DESC and defaults anything else to ASC', () => {
    const sql = makeSqlStub();
    expect(frag(repo.orderBy(sql, 'name desc')).sql).toBe('ORDER BY "name" DESC');
    expect(frag(repo.orderBy(sql, 'name bogus')).sql).toBe('ORDER BY "name" ASC');
  });

  it('composes multiple comma-separated columns', () => {
    const sql = makeSqlStub();
    const f = frag(repo.orderBy(sql, 'name ASC, createdAt DESC'));
    expect(f.sql).toBe('ORDER BY "name" ASC, "created_at" DESC');
  });

  it('rejects an unknown property (blocks identifier injection through orderBy)', () => {
    const sql = makeSqlStub();
    expect(() => repo.orderBy(sql, 'name; DROP TABLE orders')).toThrow(/Unknown property/);
    expect(() => repo.orderBy(sql, 'evil DESC')).toThrow(/Unknown property/);
  });
});

describe('Repository.identList', () => {
  const repo = new OrderRepo();

  it('emits a comma-separated quoted identifier list', () => {
    const sql = makeSqlStub();
    const f = frag(repo.idents(sql, ['id', 'created_at']));
    expect(f.sql).toBe('"id", "created_at"');
    expect(f.params).toEqual([]);
  });

  it('emits a single quoted identifier with no separator', () => {
    const sql = makeSqlStub();
    expect(frag(repo.idents(sql, ['id'])).sql).toBe('"id"');
  });

  it('returns an empty fragment for an empty name list', () => {
    const sql = makeSqlStub();
    expect(frag(repo.idents(sql, [])).sql).toBe('');
  });

  it('rejects an unsafe identifier before emitting SQL', () => {
    const sql = makeSqlStub();
    expect(() => repo.idents(sql, ['id', 'x" , (SELECT secret)'])).toThrow(/Unsafe SQL identifier/);
  });
});
