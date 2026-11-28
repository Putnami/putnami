import { RepositoryError, type RepositoryErrorCode } from '../errors';
import type { EntityHelper } from '../metadata/entity.helper';
import { assertSafeIdentifier } from '../metadata/identifier';
import { fromCamel } from '../postgres/sql-query';
import type { TableDefinition } from '../table';
import { parseConditionValue } from './query-builder';

/**
 * The statement of a `find` whose filter compares each of its properties to a
 * value, compiled once per shape: the properties it compares, in their order,
 * its ORDER BY, and whether it has an OFFSET.
 *
 * The general path composes the statement on each call from nested fragments
 * (the table and column identifiers, each condition, `WHERE`, `ORDER BY`,
 * `OFFSET`) that the driver replays into one statement. A compiled shape is
 * one template whose strings already hold the quoted identifiers: the driver
 * gets the same statement, with the same values in the same order, bound the
 * same way, from one template instead of five. An identifier is written as the
 * client's `sql(name)` builder writes it, snake_case between double quotes; a
 * safe identifier holds no quote nor dot to escape. A filter with an operator,
 * `$or` or `$and` takes the general path.
 */

/** The template of one compiled shape. */
type Strings = TemplateStringsArray;

/** The templates of one ORDER BY clause, without and with an OFFSET. */
interface OrderedShape {
  plain?: Strings;
  offset?: Strings;
}

/** The shapes that compare one sequence of properties. */
interface ShapeNode {
  /** The `"column" = ` text of each compared property, in order. */
  readonly conditions: readonly string[];
  readonly next: Map<string, ShapeNode>;
  /** Without ORDER BY, without and with an OFFSET. */
  plain: Strings | undefined;
  plainOffset: Strings | undefined;
  /** Keyed on the ORDER BY clause, not on the `orderBy` text: `id`, `id asc` and `id ,` are one entry. */
  ordered: Map<string, OrderedShape> | undefined;
}

interface ShapeRoot {
  readonly node: ShapeNode;
  /** The table identifier, quoted. */
  readonly table: string;
  /** Condition nodes held below the root: filter shapes, which the schema bounds. */
  nodes: number;
  /** ORDER BY entries held, under their own bound: request input can name clauses without end. */
  orders: number;
}

/** Condition nodes one table holds; past it, a new filter shape is compiled for its call only. */
export const MAX_FIND_NODES = 256;

/**
 * ORDER BY entries one table holds, apart from its nodes so that no `orderBy`
 * crowds a filter shape out; past it, a new clause is compiled for its call only.
 */
export const MAX_FIND_ORDERS = 256;

const roots = new WeakMap<object, ShapeRoot>();

function newNode(conditions: readonly string[]): ShapeNode {
  return { conditions, next: new Map(), plain: undefined, plainOffset: undefined, ordered: undefined };
}

/** An identifier as the client's `sql(name)` builder writes it. */
export function quoted(name: string): string {
  return `"${fromCamel(name)}"`;
}

/**
 * The template of `find(filters, { orderBy, offset })`, the condition values
 * pushed on `values` in order, or `undefined` when the filter takes the
 * general path. It throws what the general path throws for an unknown
 * property of the filter or of the ORDER BY, in the same order.
 */
export function compiledFind<T extends TableDefinition>(
  helper: EntityHelper<T>,
  filters: unknown,
  orderBy: string | undefined,
  withOffset: boolean,
  values: unknown[],
): Strings | undefined {
  if (typeof filters !== 'object' || filters === null || Array.isArray(filters)) return undefined;
  const record = filters as Record<string, unknown>;
  // The general path reads `$or` and `$and` before any other condition.
  if (record['$or'] !== undefined || record['$and'] !== undefined) return undefined;
  const root = rootOf(helper);
  const node = conditionsOf(root, helper, record, values);
  if (node === undefined) return undefined;
  const clause = orderBy ? orderByClause(helper, orderBy) : '';
  if (clause === '') {
    if (withOffset) {
      node.plainOffset ??= compile(root.table, node.conditions, '', true);
      return node.plainOffset;
    }
    node.plain ??= compile(root.table, node.conditions, '', false);
    return node.plain;
  }
  const ordered = orderedOf(root, node, clause);
  if (withOffset) {
    ordered.offset ??= compile(root.table, node.conditions, clause, true);
    return ordered.offset;
  }
  ordered.plain ??= compile(root.table, node.conditions, clause, false);
  return ordered.plain;
}

function rootOf<T extends TableDefinition>(helper: EntityHelper<T>): ShapeRoot {
  let root = roots.get(helper);
  if (root === undefined) {
    assertSafeIdentifier(helper.tableName, 'table');
    root = { node: newNode([]), table: quoted(helper.tableName), nodes: 0, orders: 0 };
    roots.set(helper, root);
  }
  return root;
}

/**
 * The node of the properties `record` compares, in order, their values pushed
 * on `values`; `undefined` when one of them holds an operator.
 */
function conditionsOf<T extends TableDefinition>(
  root: ShapeRoot,
  helper: EntityHelper<T>,
  record: Record<string, unknown>,
  values: unknown[],
): ShapeNode | undefined {
  let node = root.node;
  for (const prop in record) {
    if (!Object.hasOwn(record, prop)) continue;
    const value = record[prop];
    if (value === undefined) continue;
    if (typeof value === 'object' && value !== null && !isEquality(value)) return undefined;
    let next = node.next.get(prop);
    if (next === undefined) {
      const columnName = helper.columnName(prop);
      assertSafeIdentifier(columnName, 'where column');
      next = newNode([...node.conditions, `${quoted(columnName)} = `]);
      if (root.nodes < MAX_FIND_NODES) {
        node.next.set(prop, next);
        root.nodes++;
      }
    }
    values.push(value);
    node = next;
  }
  return node;
}

/**
 * The entry of the ORDER BY `clause` under `node`. It is keyed on the clause,
 * which the call checked against the schema: an `orderBy` from a request can
 * spell one clause in many ways, and they share one entry.
 */
function orderedOf(root: ShapeRoot, node: ShapeNode, clause: string): OrderedShape {
  let ordered = node.ordered?.get(clause);
  if (ordered === undefined) {
    ordered = {};
    if (root.orders < MAX_FIND_ORDERS) {
      node.ordered ??= new Map();
      node.ordered.set(clause, ordered);
      root.orders++;
    }
  }
  return ordered;
}

/**
 * Whether an object filter value is compared to its property as it is, as
 * the general path reads it: anything but an operator object (`{ gt: 1 }`).
 */
function isEquality(value: object): boolean {
  const conditions = parseConditionValue(value);
  const first = conditions[0];
  return conditions.length === 1 && first?.operator === '=' && first.value === value;
}

/**
 * The general path's ORDER BY as text: each `prop [ASC|DESC]` of the comma
 * list, checked against the schema in order, with the same errors.
 */
function orderByClause<T extends TableDefinition>(helper: EntityHelper<T>, orderBy: string): string {
  const columns = orderBy
    .split(',')
    .map((col) => col.trim())
    .filter(Boolean);
  const parts: string[] = [];
  for (const col of columns) {
    const words = col.split(/\s+/);
    const columnOrProp = words[0] as string;
    const direction = words[1]?.toUpperCase() || '';
    if (!helper.hasProperty(columnOrProp)) {
      throw new RepositoryError(
        `Unknown property "${columnOrProp}" in orderBy. Valid properties: ${[...helper.columnNames].join(', ')}`,
        'UNKNOWN' as RepositoryErrorCode,
      );
    }
    const columnName = helper.columnName(columnOrProp);
    assertSafeIdentifier(columnName, 'orderBy column');
    parts.push(`${quoted(columnName)} ${direction === 'DESC' ? 'DESC' : 'ASC'}`);
  }
  return parts.length > 0 ? `ORDER BY ${parts.join(', ')}` : '';
}

/**
 * `SELECT * FROM "table" [WHERE "c1" = $ AND "c2" = $ …] [ORDER BY …] LIMIT $ [OFFSET $]`
 * as template strings: one value per condition, then the limit, then the offset.
 */
function compile(table: string, conditions: readonly string[], orderBy: string, withOffset: boolean): Strings {
  const strings: string[] = [];
  let text = `SELECT * FROM ${table}`;
  for (let i = 0; i < conditions.length; i++) {
    strings.push(`${text}${i === 0 ? ' WHERE ' : ' AND '}${conditions[i]}`);
    text = '';
  }
  strings.push(`${text}${orderBy ? ` ${orderBy}` : ''} LIMIT `);
  if (withOffset) strings.push(' OFFSET ');
  strings.push('');
  return templateStrings(strings);
}

/** `strings` as a template object the driver takes: frozen, with an own `raw` that is not enumerable. */
export function templateStrings(strings: string[]): Strings {
  // A template object's `raw` is not enumerable: the driver refuses one whose `raw` is.
  Object.defineProperty(strings, 'raw', { value: Object.freeze([...strings]) });
  return Object.freeze(strings) as unknown as Strings;
}
