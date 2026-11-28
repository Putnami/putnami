import type { EntityHelper } from '../metadata/entity.helper';
import { assertSafeIdentifier } from '../metadata/identifier';
import { CapturedQuery } from '../postgres/sql-query';
import type { TableDefinition } from '../table';
import { quoted, templateStrings } from './find-statement';

/**
 * The statement of a `save`, compiled once per shape: the properties the
 * entity holds, in their order.
 *
 * The general path composes the statement on each call from nested fragments
 * (the table identifier, a row builder for the column list and the values, one
 * fragment per `SET` assignment and per conflict key) that the driver replays
 * into one statement. A compiled shape is one template whose strings already
 * hold the quoted identifiers, written as the client's `sql(name)` builder
 * writes them: the driver gets the same statement, with the same values in the
 * same order, bound the same way, from one template and no fragment. A value
 * that is itself a query of the client takes the general path: a template runs
 * such a value as part of its statement, where the row builder binds it as a
 * value.
 */

/** The template of one compiled shape. */
type Strings = TemplateStringsArray;

/** What the statement of a save writes: one column per property of the entity, in order. */
export interface SaveShape {
  /** The column of each property, in order. */
  readonly columns: readonly string[];
  /** The columns of the properties that are not keys: those a conflict updates. */
  readonly updates: readonly string[];
  /** The key columns of the table, the conflict target. */
  readonly keys: readonly string[];
}

/** The shapes that write one sequence of properties. */
interface ShapeNode extends SaveShape {
  /** The table identifier, quoted. */
  readonly table: string;
  readonly next: Map<string, ShapeNode>;
  strings: Strings | undefined;
}

interface ShapeRoot {
  readonly node: ShapeNode;
  /** The properties that are keys. */
  readonly keyProperties: ReadonlySet<string>;
  /** For each key column, the key property `extractKeys` reads its value from: the last one of that column. */
  readonly keyReads: readonly string[];
  /** Nodes held below the root: property sequences, which the schema bounds. */
  nodes: number;
}

/** Nodes one table holds; past it, a new shape is compiled for its call only. */
export const MAX_SAVE_NODES = 256;

const roots = new WeakMap<object, ShapeRoot>();

function rootOf<T extends TableDefinition>(helper: EntityHelper<T>): ShapeRoot {
  let root = roots.get(helper);
  if (root === undefined) {
    const keyProperties = helper.keyProperties;
    // `extractKeys` keys its record on the column: a later key property of
    // one column replaces an earlier one.
    const keyColumns = new Map<string, string>();
    for (const prop of keyProperties) keyColumns.set(helper.columnName(prop), prop);
    const keys = [...keyColumns.keys()];
    for (const key of keys) assertSafeIdentifier(key, 'identifier list');
    assertSafeIdentifier(helper.tableName, 'table');
    const node: ShapeNode = {
      table: quoted(helper.tableName),
      columns: [],
      updates: [],
      keys,
      next: new Map(),
      strings: undefined,
    };
    root = { node, keyProperties, keyReads: [...keyColumns.values()], nodes: 0 };
    roots.set(helper, root);
  }
  return root;
}

/**
 * Whether `entity` holds a key value, as `save` reads it: a key column whose
 * value `extractKeys` reads is neither `undefined` nor `null`.
 */
export function hasSaveKey<T extends TableDefinition>(
  helper: EntityHelper<T>,
  entity: Record<string, unknown>,
): boolean {
  const reads = rootOf(helper).keyReads;
  for (let i = 0; i < reads.length; i++) {
    const value = entity[reads[i] as string];
    if (value !== undefined && value !== null) return true;
  }
  return false;
}

/**
 * The shape of `save(entity)`, the database value of each property pushed on
 * `values` in order, `undefined` as `NULL`, as the row builder sends it. Each
 * property's column is resolved, then its `toDatabase` transform called once,
 * in order: an unknown property throws what the general path throws, at the
 * same point.
 */
export function saveShape<T extends TableDefinition>(
  helper: EntityHelper<T>,
  entity: Record<string, unknown>,
  values: unknown[],
): SaveShape {
  const root = rootOf(helper);
  let node = root.node;
  for (const prop in entity) {
    if (!Object.hasOwn(entity, prop)) continue;
    let next = node.next.get(prop);
    if (next === undefined) {
      const columnName = helper.columnName(prop);
      assertSafeIdentifier(columnName, 'insert column');
      next = {
        table: node.table,
        columns: [...node.columns, columnName],
        updates: root.keyProperties.has(prop) ? node.updates : [...node.updates, columnName],
        keys: node.keys,
        next: new Map(),
        strings: undefined,
      };
      if (root.nodes < MAX_SAVE_NODES) {
        node.next.set(prop, next);
        root.nodes++;
      }
    }
    const value = helper.toDatabaseValue(prop, entity[prop]);
    values.push(value === undefined ? null : value);
    node = next;
  }
  return node;
}

/**
 * The compiled template of `shape` for `values`, or `undefined` when they take
 * the general path: a value that is a query of the client, or no column.
 */
export function saveTemplate(shape: SaveShape, values: readonly unknown[]): Strings | undefined {
  if (shape.columns.length === 0) return undefined;
  for (let i = 0; i < values.length; i++) {
    if (values[i] instanceof CapturedQuery) return undefined;
  }
  const node = shape as ShapeNode;
  node.strings ??= compile(node);
  return node.strings;
}

/**
 * `INSERT INTO "table" ("c1", "c2" …) VALUES ($, $ …) ON CONFLICT ("k1" …) DO UPDATE SET "c2" = EXCLUDED."c2" … RETURNING *`
 * as template strings, one value per column. With no column to update, the
 * set assigns the first key to itself, so the statement still returns the row.
 */
function compile(node: ShapeNode): Strings {
  const strings = [`INSERT INTO ${node.table} (${node.columns.map(quoted).join(', ')}) VALUES (`];
  for (let i = 1; i < node.columns.length; i++) strings.push(', ');
  let set: string;
  if (node.updates.length === 0) {
    const key = node.keys[0] as string;
    assertSafeIdentifier(key, 'conflict key column');
    set = `${quoted(key)} = ${node.table}.${quoted(key)}`;
  } else {
    set = node.updates
      .map((column) => {
        assertSafeIdentifier(column, 'update column');
        return `${quoted(column)} = EXCLUDED.${quoted(column)}`;
      })
      .join(', ');
  }
  strings.push(`) ON CONFLICT (${node.keys.map(quoted).join(', ')}) DO UPDATE SET ${set} RETURNING *`);
  return templateStrings(strings);
}
