import type postgres from 'postgres';
import { assertSafeIdentifier } from '../metadata/identifier';
import { type QueryConditionValue, type QueryFilters, parseConditionValue } from './query-builder';

/**
 * Result of building a WHERE clause: a composed postgres.js fragment plus a
 * flag indicating whether any condition was emitted.
 *
 * The fragment is interpolated directly into a tagged template
 * (`sql\`... WHERE ${fragment}\``). Identifiers go through `sql(name)` and
 * values through `${value}`, so the driver parameterizes values and escapes
 * identifiers — there is no string concatenation and no `sql.unsafe`.
 */
interface WhereResult {
  fragment: postgres.PendingQuery<postgres.Row[]>;
  hasConditions: boolean;
}

/**
 * Narrow an `IN` / `NOT IN` array to the concrete primitive-array type
 * postgres.js's `sql(array)` values helper expects. Without this the driver's
 * overloads collapse to `never` for an `unknown[]` input.
 */
function toInList(value: unknown[]): readonly (string | number | boolean | Date | null)[] {
  return value as readonly (string | number | boolean | Date | null)[];
}

/**
 * Build a single SQL condition as a postgres.js fragment with operator support.
 *
 * The column name is asserted as a safe identifier and emitted via `sql(name)`;
 * the value is always parameterized. This is the single place a WHERE predicate
 * is constructed.
 */
export function buildConditionFragment(
  sql: postgres.Sql,
  columnName: string,
  operator: string,
  value: unknown,
): postgres.PendingQuery<postgres.Row[]> {
  assertSafeIdentifier(columnName, 'where column');
  const col = sql(columnName);

  switch (operator) {
    case '=':
      return sql`${col} = ${value as never}`;
    case '!=':
      return sql`${col} != ${value as never}`;
    case '>':
      return sql`${col} > ${value as never}`;
    case '<':
      return sql`${col} < ${value as never}`;
    case '>=':
      return sql`${col} >= ${value as never}`;
    case '<=':
      return sql`${col} <= ${value as never}`;
    case 'IN':
      if (Array.isArray(value) && value.length > 0) {
        return sql`${col} IN ${sql(toInList(value))}`;
      }
      // Empty IN list can never match — emit a contradiction.
      return sql`${col} IN (NULL) AND 1 = 0`;
    case 'NOT IN':
      if (Array.isArray(value) && value.length > 0) {
        return sql`${col} NOT IN ${sql(toInList(value))}`;
      }
      // Empty NOT IN excludes nothing — always true.
      return sql`1 = 1`;
    case 'LIKE':
      return sql`${col} LIKE ${value as never}`;
    case 'ILIKE':
      return sql`${col} ILIKE ${value as never}`;
    case 'IS NULL':
      return sql`${col} IS NULL`;
    case 'IS NOT NULL':
      return sql`${col} IS NOT NULL`;
    default:
      return sql`${col} = ${value as never}`;
  }
}

/** Join an array of fragments with a SQL keyword (` AND ` / ` OR `). */
function joinFragments(
  sql: postgres.Sql,
  fragments: postgres.PendingQuery<postgres.Row[]>[],
  separator: 'AND' | 'OR',
): postgres.PendingQuery<postgres.Row[]> {
  let combined = fragments[0];
  for (let i = 1; i < fragments.length; i++) {
    combined = separator === 'AND' ? sql`${combined} AND ${fragments[i]}` : sql`${combined} OR ${fragments[i]}`;
  }
  return combined;
}

/** Build the AND-joined condition fragments for a flat filter object. */
function buildGroup(
  sql: postgres.Sql,
  filter: Record<string, unknown>,
  columnNameFn: (prop: string) => string,
): postgres.PendingQuery<postgres.Row[]>[] {
  const parts: postgres.PendingQuery<postgres.Row[]>[] = [];
  for (const [prop, value] of Object.entries(filter)) {
    if (value === undefined || prop === '$or' || prop === '$and') continue;
    const columnName = columnNameFn(prop);
    for (const { operator, value: opValue } of parseConditionValue(value as QueryConditionValue)) {
      parts.push(buildConditionFragment(sql, columnName, operator, opValue));
    }
  }
  return parts;
}

/**
 * Build a WHERE clause fragment from a filters object with operator, `$or`, and
 * `$and` support.
 *
 * Replaces the former string-concatenation builder: every identifier is checked
 * with {@link assertSafeIdentifier} and emitted through `sql(name)`, and every
 * value is parameterized. Callers interpolate the returned fragment directly:
 * `sql\`SELECT * FROM ${sql(table)} WHERE ${fragment}\``.
 *
 * @param sql - The postgres.js connection used to build fragments.
 * @param filters - The filter object (property names as keys).
 * @param columnNameFn - Maps a property name to a database column name. Must
 *   reject unknown properties so attacker-controlled keys can't reach SQL.
 */
export function buildWhere<T>(
  sql: postgres.Sql,
  filters: Partial<T> | QueryFilters<T>,
  columnNameFn: (prop: string) => string,
): WhereResult {
  const record = filters as Record<string, unknown>;
  const queryFilters = filters as QueryFilters<T>;
  const conditions: postgres.PendingQuery<postgres.Row[]>[] = [];

  // $or — each entry is AND-joined internally, the entries OR-joined together.
  if (Array.isArray(queryFilters.$or)) {
    const orConditions: postgres.PendingQuery<postgres.Row[]>[] = [];
    for (const orFilter of queryFilters.$or) {
      const orParts = buildGroup(sql, orFilter as Record<string, unknown>, columnNameFn);
      if (orParts.length > 0) {
        orConditions.push(sql`(${joinFragments(sql, orParts, 'AND')})`);
      }
    }
    if (orConditions.length > 0) {
      conditions.push(sql`(${joinFragments(sql, orConditions, 'OR')})`);
    }
  }

  // $and — each entry AND-joined into its own group.
  if (Array.isArray(queryFilters.$and)) {
    for (const andFilter of queryFilters.$and) {
      const andParts = buildGroup(sql, andFilter as Record<string, unknown>, columnNameFn);
      if (andParts.length > 0) {
        conditions.push(sql`(${joinFragments(sql, andParts, 'AND')})`);
      }
    }
  }

  // Regular conditions ($or / $and keys are skipped inside buildGroup).
  conditions.push(...buildGroup(sql, record, columnNameFn));

  if (conditions.length === 0) {
    return { fragment: sql``, hasConditions: false };
  }

  return { fragment: joinFragments(sql, conditions, 'AND'), hasConditions: true };
}

/**
 * Build a WHERE fragment from already-resolved column names (used for primary
 * keys, which are column names rather than property names).
 */
export function buildWhereFromColumnNames(sql: postgres.Sql, filters: Record<string, unknown>): WhereResult {
  const entries = Object.entries(filters).filter(([, value]) => value !== undefined);

  if (entries.length === 0) {
    return { fragment: sql``, hasConditions: false };
  }

  const parts = entries.map(([columnName, value]) => buildConditionFragment(sql, columnName, '=', value));
  return { fragment: joinFragments(sql, parts, 'AND'), hasConditions: true };
}
