import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { ArrayOf, Int, Optional, resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { DocumentError, DocumentErrorCode, IndexMissing } from '../src/errors';
import { closeAllBackends } from '../src/factory';
import { Repository } from '../src/repository/repository';

function configureMemory(strictIndexes = false) {
  resetConfigLoader();
  process.env.CONFIG_DATA = JSON.stringify({
    document: {
      backend: 'memory',
      strictIndexes,
    },
  });
}

describe('Repository (memory)', () => {
  beforeEach(() => {
    configureMemory(false);
  });

  afterEach(async () => {
    await closeAllBackends();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  function usersRepository(strictIndexes = false) {
    if (strictIndexes) {
      configureMemory(true);
    }

    const collection = Collection(
      `users_${crypto.randomUUID()}`,
      {
        id: DocumentId(String),
        email: Field(String),
        name: Field(String),
        age: Field(Optional(Int)),
        tags: Field(Optional(ArrayOf(String))),
      },
      {
        indexes: [{ fields: ['email'] }, { fields: ['age'] }],
      },
    );

    return new Repository(collection);
  }

  specTest(
    'replaces documents on save by default',
    {
      feature: 'typescript/document-repository',
      requirement: 'stored-write-result',
      check: 'save-replaces-by-default',
    },
    async () => {
      const repo = usersRepository();

      await repo.save({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice',
        age: 30,
      });

      await repo.save({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice Updated',
      });

      expect(await repo.get('user-1')).toEqual({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice Updated',
      });
    },
  );

  specTest(
    'supports merge writes when requested',
    {
      feature: 'typescript/document-repository',
      requirement: 'stored-write-result',
      check: 'a-merge-write-is-explicit',
    },
    async () => {
      const repo = usersRepository();

      await repo.save({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice',
        age: 30,
      });

      await repo.save(
        {
          id: 'user-1',
          name: 'Alice Updated',
        },
        { merge: true },
      );

      expect(await repo.get('user-1')).toEqual({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice Updated',
        age: 30,
      });
    },
  );

  specTest(
    'supports find operators and pagination results',
    {
      feature: 'typescript/document-repository',
      requirement: 'portable-query',
      check: 'find-operators-and-pagination-results-are-supported',
    },
    async () => {
      const repo = usersRepository();

      await repo.saveMany([
        {
          id: 'user-1',
          email: 'alice@example.com',
          name: 'Alice',
          age: 30,
          tags: ['vip', 'team-a'],
        },
        {
          id: 'user-2',
          email: 'bob@example.com',
          name: 'Bob',
          age: 19,
          tags: ['team-b'],
        },
        {
          id: 'user-3',
          email: 'charlie@example.com',
          name: 'Charlie',
        },
      ]);

      expect(await repo.findOne({ email: 'alice@example.com' })).toEqual({
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice',
        age: 30,
        tags: ['vip', 'team-a'],
      });

      const adults = await repo.find(
        {
          age: { gte: 21 },
          tags: { contains: 'vip' },
        },
        { orderBy: { field: 'age', direction: 'asc' } },
      );
      expect(adults.items.map((item) => item.id)).toEqual(['user-1']);

      const existingAge = await repo.find({ age: { exists: true } });
      expect(existingAge.items.map((item) => item.id).sort()).toEqual(['user-1', 'user-2']);

      const noAge = await repo.find({ age: { exists: false } });
      expect(noAge.items.map((item) => item.id)).toEqual(['user-3']);

      const notBob = await repo.find({ email: { notIn: ['bob@example.com'] } });
      expect(notBob.items.map((item) => item.id).sort()).toEqual(['user-1', 'user-3']);
    },
  );

  it('deletes single and multiple documents', async () => {
    const repo = usersRepository();

    await repo.saveMany([
      { id: 'user-1', email: 'alice@example.com', name: 'Alice', age: 30 },
      { id: 'user-2', email: 'bob@example.com', name: 'Bob', age: 19 },
      { id: 'user-3', email: 'charlie@example.com', name: 'Charlie', age: 19 },
    ]);

    expect(await repo.delete('user-1')).toEqual({
      success: true,
      item: {
        id: 'user-1',
        email: 'alice@example.com',
        name: 'Alice',
        age: 30,
      },
    });

    expect(await repo.deleteMany({ age: 19 })).toBe(2);
    expect((await repo.find()).items).toEqual([]);
  });

  specTest(
    'treats limit:0 as zero rows instead of an unbounded scan',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'a-zero-limit-means-zero-rows',
    },
    async () => {
      const repo = usersRepository();

      await repo.saveMany([
        { id: 'user-1', email: 'alice@example.com', name: 'Alice', age: 30 },
        { id: 'user-2', email: 'bob@example.com', name: 'Bob', age: 19 },
      ]);

      const none = await repo.find({}, { limit: 0 });
      expect(none.items).toEqual([]);

      const noneByFilter = await repo.find(
        { age: { gte: 0 } },
        { limit: 0, orderBy: { field: 'age', direction: 'asc' } },
      );
      expect(noneByFilter.items).toEqual([]);
    },
  );

  specTest(
    'falls back to the default page size for absent, negative, and NaN limits',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'an-absent-or-invalid-limit-falls-back-to-the-default-page-size',
    },
    async () => {
      const repo = usersRepository();

      await repo.saveMany(
        Array.from({ length: 5 }, (_, index) => ({
          id: `user-${index}`,
          email: `user-${index}@example.com`,
          name: `User ${index}`,
          age: index,
        })),
      );

      // undefined -> default page size (returns everything for this small set)
      expect((await repo.find({})).items.length).toBe(5);
      // negative -> default page size (not collapsed to 0)
      expect((await repo.find({}, { limit: -5 })).items.length).toBe(5);
      // NaN -> default page size
      expect((await repo.find({}, { limit: Number.NaN })).items.length).toBe(5);
    },
  );

  specTest(
    'honors a positive limit exactly',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'a-positive-limit-is-honored-exactly',
    },
    async () => {
      const repo = usersRepository();

      await repo.saveMany(
        Array.from({ length: 5 }, (_, index) => ({
          id: `user-${index}`,
          email: `user-${index}@example.com`,
          name: `User ${index}`,
          age: index,
        })),
      );

      const page = await repo.find({}, { limit: 2, orderBy: { field: 'age', direction: 'asc' } });
      expect(page.items.map((item) => item.id)).toEqual(['user-0', 'user-1']);
    },
  );

  it('enforces declared indexes when strictIndexes is enabled', async () => {
    const repo = usersRepository(true);

    await repo.save({
      id: 'user-1',
      email: 'alice@example.com',
      name: 'Alice',
    });

    try {
      await repo.find({ name: 'Alice' });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(IndexMissing);
    }
  });

  specTest(
    'refuses deleteMany with an empty filter object for safety',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'delete-many-refuses-an-empty-filter',
    },
    async () => {
      const repo = usersRepository();

      await repo.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice', age: 30 });

      try {
        await repo.deleteMany({});
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.DeleteWithoutFilters);
      }

      // The guard must not delete anything before throwing.
      expect((await repo.find()).items.map((item) => item.id)).toEqual(['user-1']);
    },
  );

  specTest(
    'refuses deleteMany when every filter value is undefined',
    {
      feature: 'typescript/document-repository',
      requirement: 'bounded-bulk-behavior',
      check: 'delete-many-refuses-an-all-undefined-filter',
    },
    async () => {
      const repo = usersRepository();

      await repo.save({ id: 'user-1', email: 'alice@example.com', name: 'Alice', age: 30 });

      try {
        await repo.deleteMany({ age: undefined, email: undefined });
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.DeleteWithoutFilters);
      }

      expect((await repo.find()).items.map((item) => item.id)).toEqual(['user-1']);
    },
  );

  specTest(
    'throws EmptyDocument when saving an empty document',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'an-empty-document-is-rejected',
    },
    async () => {
      const repo = usersRepository();

      try {
        await repo.save({});
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.EmptyDocument);
      }
    },
  );

  specTest(
    'throws ValidationError when saving a schema-invalid document',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'a-schema-invalid-save-is-rejected-at-the-boundary',
    },
    async () => {
      const repo = usersRepository();

      try {
        // email is a required String field; a number violates the schema.
        await repo.save({ id: 'user-1', email: 123 as unknown as string, name: 'Alice' });
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.ValidationError);
      }
    },
  );

  specTest(
    'throws ValidationError when a replace save is missing a required field',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'a-replace-save-missing-a-required-field-is-rejected',
    },
    async () => {
      const repo = usersRepository();

      try {
        // name is required for a full (replace) write.
        await repo.save({ id: 'user-1', email: 'alice@example.com' });
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.ValidationError);
      }
    },
  );

  specTest(
    'throws EmptyDocuments when saveMany receives an empty array',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'save-many-rejects-an-empty-array',
    },
    async () => {
      const repo = usersRepository();

      try {
        await repo.saveMany([]);
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.EmptyDocuments);
      }
    },
  );

  specTest(
    'throws EmptyDocument when saveMany contains an empty document',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'save-many-rejects-an-empty-document',
    },
    async () => {
      const repo = usersRepository();

      try {
        await repo.saveMany([{ id: 'user-1', email: 'alice@example.com', name: 'Alice' }, {}]);
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.EmptyDocument);
      }
    },
  );

  specTest(
    'throws ValidationError when saveMany contains a schema-invalid document',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'save-many-rejects-a-schema-invalid-document',
    },
    async () => {
      const repo = usersRepository();

      try {
        await repo.saveMany([
          { id: 'user-1', email: 'alice@example.com', name: 'Alice' },
          { id: 'user-2', email: 'bob@example.com', name: 42 as unknown as string },
        ]);
        expect.unreachable();
      } catch (error) {
        expect(error).toBeInstanceOf(DocumentError);
        expect((error as DocumentError).code).toBe(DocumentErrorCode.ValidationError);
      }
    },
  );
});
