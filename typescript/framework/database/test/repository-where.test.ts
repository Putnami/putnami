import { describe, expect, it } from 'bun:test';
import type postgres from 'postgres';
import { buildConditionFragment, buildWhere, buildWhereFromColumnNames } from '../src/repository/repository-where';
import { parseConditionValue } from '../src/repository/query-builder';

/**
 * A DB-free stand-in for the postgres.js tagged template + its `sql(value)`
 * helper, sufficient to exercise the WHERE/condition builders without a
 * connection. It records every emitted fragment so tests can assert:
 *
 *  - the SQL *shape* (identifiers quoted, operators/keywords literal);
 *  - that every value is *parameterized* — captured in `params`, rendered as a
 *    `?` placeholder, never interpolated into the SQL text;
 *  - that nested fragments splice in (matching postgres.js, where interpolating
 *    a `PendingQuery` inlines its SQL rather than treating it as a bind value).
 *
 * Two node shapes flow through interpolation:
 *  - an *identifier* (`sql('col')`) → rendered as `"col"`, an escaped name;
 *  - a *value list* (`sql([1,2])`) → rendered as `(?, ?)`, all parameterized.
 * Anything else interpolated via `${...}` is a bind value and pushed to params.
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

/**
 * Build a stub `sql` tag. The function form mirrors postgres.js: `sql(name)` is
 * an identifier helper, `sql(array)` is a values-list helper. The tagged-template
 * form composes a {@link Fragment}; interpolating a node splices it, anything
 * else is parameterized.
 */
function makeSqlStub(): postgres.Sql {
  const tag = (strings: TemplateStringsArray | string, ...values: unknown[]): unknown => {
    // Helper-call form: sql('col') or sql([1, 2, 3]).
    if (!Array.isArray(strings) || !('raw' in (strings as TemplateStringsArray))) {
      if (Array.isArray(strings)) {
        return { kind: 'valuelist', values: strings } satisfies ValueList;
      }
      return { kind: 'ident', name: strings as string } satisfies Ident;
    }

    // Tagged-template form.
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
            // Splice a nested fragment: inline its SQL and carry its params.
            sql += v.sql;
            params.push(...v.params);
          }
        } else {
          // A bind value — never interpolated into the SQL text.
          sql += '?';
          params.push(v);
        }
      }
    }
    return { kind: 'fragment', sql, params } satisfies Fragment;
  };
  return tag as unknown as postgres.Sql;
}

/** Read a built fragment's recorded SQL + params. */
function frag(value: postgres.PendingQuery<postgres.Row[]>): Fragment {
  return value as unknown as Fragment;
}

/** Identity property→column mapper that rejects unknown keys (the injection guard). */
function columnMapper(known: string[]): (prop: string) => string {
  const set = new Set(known);
  return (prop) => {
    if (!set.has(prop)) {
      throw new Error(`Unknown property "${prop}"`);
    }
    return prop;
  };
}

describe('buildConditionFragment', () => {
  it('quotes the column identifier and parameterizes the value for each comparison operator', () => {
    const sql = makeSqlStub();
    for (const op of ['=', '!=', '>', '<', '>=', '<=', 'LIKE', 'ILIKE']) {
      const f = frag(buildConditionFragment(sql, 'age', op, 18));
      expect(f.sql).toBe(`"age" ${op} ?`);
      expect(f.params).toEqual([18]);
    }
  });

  it('rejects an unsafe column identifier before emitting any SQL', () => {
    const sql = makeSqlStub();
    expect(() => buildConditionFragment(sql, 'name" = name OR "1"="1', '=', 'x')).toThrow(/Unsafe SQL identifier/);
    expect(() => buildConditionFragment(sql, 'a; DROP TABLE t', '=', 'x')).toThrow(/Unsafe SQL identifier/);
  });

  it('parameterizes every element of an IN list (no interpolation)', () => {
    const sql = makeSqlStub();
    const f = frag(buildConditionFragment(sql, 'status', 'IN', ['active', 'pending']));
    expect(f.sql).toBe('"status" IN (?, ?)');
    expect(f.params).toEqual(['active', 'pending']);
  });

  it('emits an always-false contradiction for an empty IN list', () => {
    const sql = makeSqlStub();
    const f = frag(buildConditionFragment(sql, 'status', 'IN', []));
    expect(f.sql).toBe('"status" IN (NULL) AND 1 = 0');
    expect(f.params).toEqual([]);
  });

  it('parameterizes every element of a NOT IN list', () => {
    const sql = makeSqlStub();
    const f = frag(buildConditionFragment(sql, 'status', 'NOT IN', ['banned']));
    expect(f.sql).toBe('"status" NOT IN (?)');
    expect(f.params).toEqual(['banned']);
  });

  it('emits an always-true tautology for an empty NOT IN list', () => {
    const sql = makeSqlStub();
    const f = frag(buildConditionFragment(sql, 'status', 'NOT IN', []));
    expect(f.sql).toBe('1 = 1');
    expect(f.params).toEqual([]);
  });

  it('emits IS NULL / IS NOT NULL with no bind parameter', () => {
    const sql = makeSqlStub();
    expect(frag(buildConditionFragment(sql, 'deleted_at', 'IS NULL', null)).sql).toBe('"deleted_at" IS NULL');
    expect(frag(buildConditionFragment(sql, 'deleted_at', 'IS NULL', null)).params).toEqual([]);
    expect(frag(buildConditionFragment(sql, 'deleted_at', 'IS NOT NULL', null)).sql).toBe('"deleted_at" IS NOT NULL');
  });

  it('falls back to equality (still parameterized) for an unknown operator', () => {
    const sql = makeSqlStub();
    const f = frag(buildConditionFragment(sql, 'age', 'BOGUS', 7));
    expect(f.sql).toBe('"age" = ?');
    expect(f.params).toEqual([7]);
  });

  it('parameterizes a string that contains SQL metacharacters instead of inlining it', () => {
    const sql = makeSqlStub();
    const evil = "x'; DROP TABLE users; --";
    const f = frag(buildConditionFragment(sql, 'name', '=', evil));
    expect(f.sql).toBe('"name" = ?');
    expect(f.params).toEqual([evil]);
    // The dangerous string never reaches the SQL text.
    expect(f.sql).not.toContain('DROP TABLE');
  });
});

describe('parseConditionValue', () => {
  it('treats a bare value as equality', () => {
    expect(parseConditionValue('Alice')).toEqual([{ operator: '=', value: 'Alice' }]);
    expect(parseConditionValue(42)).toEqual([{ operator: '=', value: 42 }]);
  });

  it('maps each Prisma-style operator key to its SQL operator', () => {
    expect(parseConditionValue({ gt: 18 })).toEqual([{ operator: '>', value: 18 }]);
    expect(parseConditionValue({ lt: 5 })).toEqual([{ operator: '<', value: 5 }]);
    expect(parseConditionValue({ gte: 1 })).toEqual([{ operator: '>=', value: 1 }]);
    expect(parseConditionValue({ lte: 9 })).toEqual([{ operator: '<=', value: 9 }]);
    expect(parseConditionValue({ not: 'x' })).toEqual([{ operator: '!=', value: 'x' }]);
    expect(parseConditionValue({ equals: 'y' })).toEqual([{ operator: '=', value: 'y' }]);
    expect(parseConditionValue({ like: '%a%' })).toEqual([{ operator: 'LIKE', value: '%a%' }]);
    expect(parseConditionValue({ ilike: '%b%' })).toEqual([{ operator: 'ILIKE', value: '%b%' }]);
    expect(parseConditionValue({ in: [1, 2] })).toEqual([{ operator: 'IN', value: [1, 2] }]);
    expect(parseConditionValue({ notIn: [3] })).toEqual([{ operator: 'NOT IN', value: [3] }]);
  });

  it('maps isNull / isNotNull to null-valued conditions only when true', () => {
    expect(parseConditionValue({ isNull: true })).toEqual([{ operator: 'IS NULL', value: null }]);
    expect(parseConditionValue({ isNotNull: true })).toEqual([{ operator: 'IS NOT NULL', value: null }]);
    // isNull: false contributes no condition; with no other keys it degrades to equality on the object.
    const res = parseConditionValue({ isNull: false } as never);
    expect(res).toEqual([{ operator: '=', value: { isNull: false } }]);
  });

  it('produces multiple conditions for a range object (AND-composed by the caller)', () => {
    expect(parseConditionValue({ gte: 100, lte: 500 })).toEqual([
      { operator: '>=', value: 100 },
      { operator: '<=', value: 500 },
    ]);
  });

  it('treats a plain object with no operator keys as an equality value', () => {
    const obj = { nested: 1 };
    expect(parseConditionValue(obj as never)).toEqual([{ operator: '=', value: obj }]);
  });

  it('treats an array value as equality (not an operator object)', () => {
    expect(parseConditionValue([1, 2, 3] as never)).toEqual([{ operator: '=', value: [1, 2, 3] }]);
  });
});

describe('buildWhere', () => {
  it('returns hasConditions=false and an empty fragment for an empty filter', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, {}, columnMapper(['name']));
    expect(result.hasConditions).toBe(false);
    expect(frag(result.fragment).sql).toBe('');
  });

  it('AND-joins flat equality conditions and parameterizes every value', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, { name: 'Alice', currency: 'USD' }, columnMapper(['name', 'currency']));
    expect(result.hasConditions).toBe(true);
    const f = frag(result.fragment);
    expect(f.sql).toBe('"name" = ? AND "currency" = ?');
    expect(f.params).toEqual(['Alice', 'USD']);
  });

  it('skips undefined values', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, { name: 'Alice', currency: undefined }, columnMapper(['name', 'currency']));
    expect(frag(result.fragment).sql).toBe('"name" = ?');
    expect(frag(result.fragment).params).toEqual(['Alice']);
  });

  it('rejects an unknown filter key via the column mapper (injection guard)', () => {
    const sql = makeSqlStub();
    expect(() => buildWhere(sql, { evil: 1 } as never, columnMapper(['name']))).toThrow(/Unknown property "evil"/);
  });

  it('expands operator objects into parameterized comparisons', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, { age: { gte: 18, lte: 65 } } as never, columnMapper(['age']));
    const f = frag(result.fragment);
    expect(f.sql).toBe('"age" >= ? AND "age" <= ?');
    expect(f.params).toEqual([18, 65]);
  });

  it('composes $or as OR-joined AND-groups', () => {
    const sql = makeSqlStub();
    const result = buildWhere(
      sql,
      { $or: [{ name: 'Alice' }, { name: 'Bob', currency: 'EUR' }] } as never,
      columnMapper(['name', 'currency']),
    );
    const f = frag(result.fragment);
    expect(f.sql).toBe('(("name" = ?) OR ("name" = ? AND "currency" = ?))');
    expect(f.params).toEqual(['Alice', 'Bob', 'EUR']);
  });

  it('composes $and as separate AND-joined groups', () => {
    const sql = makeSqlStub();
    const result = buildWhere(
      sql,
      { $and: [{ name: 'Alice' }, { currency: 'USD' }] } as never,
      columnMapper(['name', 'currency']),
    );
    const f = frag(result.fragment);
    expect(f.sql).toBe('("name" = ?) AND ("currency" = ?)');
    expect(f.params).toEqual(['Alice', 'USD']);
  });

  it('combines $or, $and and regular conditions, AND-joined at the top level', () => {
    const sql = makeSqlStub();
    const result = buildWhere(
      sql,
      {
        currency: 'USD',
        $or: [{ name: 'Alice' }, { name: 'Bob' }],
        $and: [{ symbol: 'BTC' }],
      } as never,
      columnMapper(['name', 'currency', 'symbol']),
    );
    const f = frag(result.fragment);
    // Order: $or group, then $and group, then the regular (currency) condition.
    expect(f.sql).toBe('(("name" = ?) OR ("name" = ?)) AND ("symbol" = ?) AND "currency" = ?');
    expect(f.params).toEqual(['Alice', 'Bob', 'BTC', 'USD']);
  });

  it('parameterizes an IN operator inside a filter', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, { currency: { in: ['USD', 'EUR'] } } as never, columnMapper(['currency']));
    const f = frag(result.fragment);
    expect(f.sql).toBe('"currency" IN (?, ?)');
    expect(f.params).toEqual(['USD', 'EUR']);
  });

  it('drops an empty $or (no spurious condition) → hasConditions=false', () => {
    const sql = makeSqlStub();
    const result = buildWhere(sql, { $or: [] } as never, columnMapper(['name']));
    expect(result.hasConditions).toBe(false);
    expect(frag(result.fragment).sql).toBe('');
  });
});

describe('buildWhereFromColumnNames', () => {
  it('AND-joins resolved column equalities and parameterizes their values', () => {
    const sql = makeSqlStub();
    const result = buildWhereFromColumnNames(sql, { id: 'abc', tenant_id: 7 });
    expect(result.hasConditions).toBe(true);
    const f = frag(result.fragment);
    expect(f.sql).toBe('"id" = ? AND "tenant_id" = ?');
    expect(f.params).toEqual(['abc', 7]);
  });

  it('skips undefined values and returns no conditions when all are undefined', () => {
    const sql = makeSqlStub();
    const result = buildWhereFromColumnNames(sql, { id: undefined });
    expect(result.hasConditions).toBe(false);
    expect(frag(result.fragment).sql).toBe('');
  });

  it('rejects an unsafe column name', () => {
    const sql = makeSqlStub();
    expect(() => buildWhereFromColumnNames(sql, { 'id; DROP TABLE t': 1 })).toThrow(/Unsafe SQL identifier/);
  });
});
