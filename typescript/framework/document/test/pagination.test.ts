import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { Int, resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { closeAllBackends } from '../src/factory';
import { Repository } from '../src/repository/repository';

describe('cursor pagination', () => {
  beforeEach(() => {
    resetConfigLoader();
    process.env.CONFIG_DATA = JSON.stringify({
      document: {
        backend: 'memory',
      },
    });
  });

  afterEach(async () => {
    await closeAllBackends();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  specTest(
    'pages deterministically with a stable id tie-breaker',
    {
      feature: 'typescript/document-repository',
      requirement: 'portable-query',
      check: 'paging-is-deterministic-with-the-id-tie-breaker',
    },
    async () => {
      const repo = new Repository(
        Collection(`scores_${crypto.randomUUID()}`, {
          id: DocumentId(String),
          score: Field(Int),
        }),
      );

      await repo.saveMany([
        { id: 'a', score: 1 },
        { id: 'b', score: 1 },
        { id: 'c', score: 2 },
        { id: 'd', score: 2 },
      ]);

      const page1 = await repo.find({}, { limit: 2, orderBy: { field: 'score', direction: 'asc' } });
      expect(page1.items.map((item) => item.id)).toEqual(['a', 'b']);
      expect(page1.nextCursor).toBeDefined();

      const page2 = await repo.find(
        {},
        {
          limit: 2,
          cursor: page1.nextCursor,
          orderBy: { field: 'score', direction: 'asc' },
        },
      );

      expect(page2.items.map((item) => item.id)).toEqual(['c', 'd']);
      expect(page2.nextCursor).toBeUndefined();
    },
  );
});
