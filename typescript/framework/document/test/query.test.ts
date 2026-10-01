import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { normalizeOrderBy, parseQueryFilters } from '../src/repository/query';

describe('query helpers', () => {
  specTest(
    'parses the portable operator subset',
    {
      feature: 'typescript/document-repository',
      requirement: 'portable-query',
      check: 'only-the-documented-operator-subset-is-parsed',
    },
    () => {
      const filters = parseQueryFilters(
        {
          age: { gte: 18, lt: 65 },
          tags: { contains: 'vip' },
          active: { exists: true },
          name: { not: 'blocked' },
        },
        (property) => property,
        () => {},
      );

      expect(filters).toEqual([
        { field: 'age', op: 'gte', value: 18 },
        { field: 'age', op: 'lt', value: 65 },
        { field: 'tags', op: 'contains', value: 'vip' },
        { field: 'active', op: 'exists', value: true },
        { field: 'name', op: 'ne', value: 'blocked' },
      ]);
    },
  );

  specTest(
    'appends document id fields to orderBy for stable pagination',
    {
      feature: 'typescript/document-repository',
      requirement: 'portable-query',
      check: 'document-id-fields-are-appended-as-ordering-tie-breakers',
    },
    () => {
      const orderBy = normalizeOrderBy(
        { field: 'score', direction: 'desc' },
        (property) => property,
        () => {},
        ['id'],
      );

      expect(orderBy).toEqual([
        { field: 'score', direction: 'desc' },
        { field: 'id', direction: 'asc' },
      ]);
    },
  );
});
