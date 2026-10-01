import type { AdapterFilter, AdapterOp, AdapterOrderBy } from '../adapter/document.adapter';
import { DocumentError, DocumentErrorCode } from '../errors';

export type Consistency = 'strong' | 'eventual';

export type QueryFilterOperators<V> = {
  equals?: V;
  not?: V;
  gt?: V;
  gte?: V;
  lt?: V;
  lte?: V;
  in?: V[];
  notIn?: V[];
  contains?: V extends (infer U)[] ? U : never;
  exists?: boolean;
};

export type QueryConditionValue<V> = V | QueryFilterOperators<V>;

export type QueryFilters<T> = {
  [K in keyof T]?: QueryConditionValue<T[K]>;
};

export interface FindOrder<ENTITY = Record<string, unknown>> {
  field: Extract<keyof ENTITY, string>;
  direction?: 'asc' | 'desc';
}

export interface FindOptions<ENTITY = Record<string, unknown>> {
  limit?: number;
  cursor?: string;
  orderBy?: FindOrder<ENTITY> | readonly FindOrder<ENTITY>[];
  consistency?: Consistency;
}

export interface FindResult<T> {
  items: T[];
  nextCursor?: string;
}

const FILTER_OPERATOR_KEYS = new Set(['equals', 'not', 'gt', 'gte', 'lt', 'lte', 'in', 'notIn', 'contains', 'exists']);

function isFilterOperators(value: unknown): value is QueryFilterOperators<unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  return Object.keys(value).some((key) => FILTER_OPERATOR_KEYS.has(key));
}

function pushFilter(filters: AdapterFilter[], field: string, op: AdapterOp, value: unknown): void {
  filters.push({ field, op, value });
}

export function parseQueryFilters<T>(
  queryFilters: QueryFilters<T>,
  fieldName: (property: string) => string,
  assertProperty: (property: string) => void,
): AdapterFilter[] {
  const filters: AdapterFilter[] = [];

  for (const [property, value] of Object.entries(queryFilters as Record<string, unknown>)) {
    if (value === undefined) continue;
    assertProperty(property);
    const resolvedField = fieldName(property);

    if (isFilterOperators(value)) {
      const ops = value as QueryFilterOperators<unknown>;
      if (ops.equals !== undefined) pushFilter(filters, resolvedField, 'eq', ops.equals);
      if (ops.not !== undefined) pushFilter(filters, resolvedField, 'ne', ops.not);
      if (ops.gt !== undefined) pushFilter(filters, resolvedField, 'gt', ops.gt);
      if (ops.gte !== undefined) pushFilter(filters, resolvedField, 'gte', ops.gte);
      if (ops.lt !== undefined) pushFilter(filters, resolvedField, 'lt', ops.lt);
      if (ops.lte !== undefined) pushFilter(filters, resolvedField, 'lte', ops.lte);
      if (ops.in !== undefined) pushFilter(filters, resolvedField, 'in', ops.in);
      if (ops.notIn !== undefined) pushFilter(filters, resolvedField, 'notIn', ops.notIn);
      if (ops.contains !== undefined) pushFilter(filters, resolvedField, 'contains', ops.contains);
      if (ops.exists !== undefined) pushFilter(filters, resolvedField, 'exists', ops.exists);
      continue;
    }

    pushFilter(filters, resolvedField, 'eq', value);
  }

  return filters;
}

export function normalizeOrderBy<ENTITY>(
  orderBy: FindOptions<ENTITY>['orderBy'],
  fieldName: (property: string) => string,
  assertProperty: (property: string) => void,
  fallbackProperties: string[],
): AdapterOrderBy[] {
  const normalized: AdapterOrderBy[] = [];
  const input = Array.isArray(orderBy) ? orderBy : orderBy ? [orderBy] : [];

  for (const item of input) {
    const property = item.field as string;
    assertProperty(property);
    normalized.push({
      field: fieldName(property),
      direction: item.direction ?? 'asc',
    });
  }

  for (const property of fallbackProperties) {
    const field = fieldName(property);
    if (!normalized.some((item) => item.field === field)) {
      normalized.push({ field, direction: 'asc' });
    }
  }

  if (normalized.length === 0) {
    throw new DocumentError(
      'Document collections must declare at least one DocumentId field',
      DocumentErrorCode.MissingDocumentId,
    );
  }

  return normalized;
}
