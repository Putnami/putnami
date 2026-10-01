/**
 * Query operator types for advanced filtering (internal)
 */
export type QueryOperator =
  | '='
  | '!='
  | '>'
  | '<'
  | '>='
  | '<='
  | 'IN'
  | 'NOT IN'
  | 'LIKE'
  | 'ILIKE'
  | 'IS NULL'
  | 'IS NOT NULL';

/**
 * Prisma-style filter operators for a field value.
 */
export type QueryFilterOperators<T = unknown> = {
  equals?: T;
  not?: T;
  gt?: T;
  lt?: T;
  gte?: T;
  lte?: T;
  in?: T[];
  notIn?: T[];
  like?: string;
  ilike?: string;
  isNull?: boolean;
  isNotNull?: boolean;
};

const FILTER_OPERATOR_KEYS = new Set([
  'equals',
  'not',
  'gt',
  'lt',
  'gte',
  'lte',
  'in',
  'notIn',
  'like',
  'ilike',
  'isNull',
  'isNotNull',
]);

/**
 * Query condition value - can be a simple value or a Prisma-style operator object.
 *
 * @example
 * // Simple equality
 * { name: 'Alice' }
 *
 * // Comparison operators
 * { age: { gt: 18 } }
 * { price: { gte: 100, lte: 500 } }
 *
 * // List operators
 * { status: { in: ['active', 'pending'] } }
 *
 * // Pattern matching
 * { name: { like: '%test%' } }
 * { name: { ilike: '%test%' } }
 *
 * // Null checks
 * { deletedAt: { isNull: true } }
 */
export type QueryConditionValue<T = unknown> = T | QueryFilterOperators<T>;

/**
 * Query filters with support for operators
 */
export type QueryFilters<ENTITY> = {
  [K in keyof ENTITY]?: QueryConditionValue<ENTITY[K]>;
} & {
  $or?: Partial<ENTITY>[];
  $and?: Partial<ENTITY>[];
};

function isFilterOperators(value: unknown): value is QueryFilterOperators {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  return Object.keys(value).some((k) => FILTER_OPERATOR_KEYS.has(k));
}

/**
 * Parse a query condition value into one or more operator/value pairs.
 * Supports Prisma-style operators: equals, not, gt, lt, gte, lte, in, notIn, like, ilike, isNull, isNotNull.
 * Multiple operators on the same field produce multiple conditions (AND).
 */
export function parseConditionValue<T>(value: QueryConditionValue<T>): {
  operator: QueryOperator;
  value: T | T[] | null;
}[] {
  if (isFilterOperators(value)) {
    const ops = value as QueryFilterOperators<T>;
    const conditions: { operator: QueryOperator; value: T | T[] | null }[] = [];

    if (ops.equals !== undefined) conditions.push({ operator: '=', value: ops.equals });
    if (ops.not !== undefined) conditions.push({ operator: '!=', value: ops.not });
    if (ops.gt !== undefined) conditions.push({ operator: '>', value: ops.gt });
    if (ops.lt !== undefined) conditions.push({ operator: '<', value: ops.lt });
    if (ops.gte !== undefined) conditions.push({ operator: '>=', value: ops.gte });
    if (ops.lte !== undefined) conditions.push({ operator: '<=', value: ops.lte });
    if (ops.in !== undefined) conditions.push({ operator: 'IN', value: ops.in as T[] });
    if (ops.notIn !== undefined) conditions.push({ operator: 'NOT IN', value: ops.notIn as T[] });
    if (ops.like !== undefined) conditions.push({ operator: 'LIKE', value: ops.like as T });
    if (ops.ilike !== undefined) conditions.push({ operator: 'ILIKE', value: ops.ilike as T });
    if (ops.isNull === true) conditions.push({ operator: 'IS NULL', value: null });
    if (ops.isNotNull === true) conditions.push({ operator: 'IS NOT NULL', value: null });

    if (conditions.length === 0) {
      return [{ operator: '=', value: value as T }];
    }
    return conditions;
  }

  return [{ operator: '=', value: value as T }];
}
