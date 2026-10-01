import { type SchemaPrimitive, ArrayOf } from '@putnami/runtime';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';

import type { DatabaseValue } from '../type.type';
import type {
  ColumnDefinition,
  ColumnOptions,
  KeyDefinition,
  KeyOptions,
  TableDefinition,
  TableOptions,
  TableSchema,
} from './table-definition';

const TABLE_MARKER = 'putnami:table' as const;
const COLUMN_MARKER = 'putnami:column' as const;
const KEY_MARKER = 'putnami:key' as const;

/** Map a schema primitive to its PostgreSQL array column type. */
function pgArrayColumnType(itemType: SchemaPrimitive): string {
  if (itemType === String) return 'TEXT[]';
  if (itemType === Number) return 'DOUBLE PRECISION[]';
  if (itemType === Boolean) return 'BOOLEAN[]';
  throw new Error(`Unsupported PgArray item type: ${String(itemType)}`);
}

/**
 * Define a database column.
 */
export function Column<P extends SchemaPrimitive>(type: P, options?: ColumnOptions): ColumnDefinition<P> {
  return {
    __column: COLUMN_MARKER,
    type,
    options: options ?? {},
  };
}

/**
 * Define a column backed by a native PostgreSQL array (e.g., `TEXT[]`, `INT[]`).
 *
 * Unlike `Column(ArrayOf(T))` which stores as JSONB, `PgArray` uses native
 * PostgreSQL array types for idiomatic storage and GIN indexing.
 *
 * @param itemType - The element type (`String`, `Number`, or `Boolean`)
 * @param options  - Column options (columnName, default, etc.)
 *
 * @example
 * ```ts
 * const ClientsTable = Table('oauth_clients', {
 *   id:           Key(Uuid),
 *   redirectUris: PgArray(String, { columnName: 'redirect_uris' }),
 *   grantTypes:   PgArray(String, { columnName: 'grant_types' }),
 *   scopes:       PgArray(String),
 * });
 * // TypeScript type: { redirectUris: string[]; grantTypes: string[]; scopes: string[] }
 * // SQL: redirect_uris TEXT[] NOT NULL, grant_types TEXT[] NOT NULL, scopes TEXT[] NOT NULL
 * ```
 */
export function PgArray<P extends SchemaPrimitive>(
  itemType: P,
  options?: Omit<ColumnOptions, 'columnType' | 'toDatabase' | 'fromDatabase'>,
): ColumnDefinition<ReturnType<typeof ArrayOf<P>>> {
  const arraySchema = ArrayOf(itemType);
  return {
    __column: COLUMN_MARKER,
    type: arraySchema,
    options: {
      ...options,
      columnType: pgArrayColumnType(itemType),
      toDatabase: (value: unknown) => (Array.isArray(value) ? value : []) as unknown as DatabaseValue,
      fromDatabase: (value: unknown) => (Array.isArray(value) ? value : []),
    },
  };
}

/**
 * Define a primary key column.
 */
export function Key<P extends SchemaPrimitive>(type: P, options?: KeyOptions): KeyDefinition<P> {
  return {
    __key: KEY_MARKER,
    type,
    options: options ?? {},
  };
}

/**
 * Define a database table.
 *
 * @param tableName - The database table name
 * @param schema - Column and key definitions
 * @param options - Table-level options (db, schema)
 */
export function Table<S extends TableSchema>(tableName: string, schema: S, options?: TableOptions): TableDefinition<S> {
  const caller = getExternalCaller(getProjectRoot());
  return {
    __table: TABLE_MARKER,
    tableName,
    schema,
    options: options ?? {},
    ...(caller
      ? {
          __source: {
            path: caller.filePath,
            line: caller.lineNumber,
            ...(caller.functionName ? { symbol: caller.functionName } : {}),
          },
        }
      : {}),
  };
}
