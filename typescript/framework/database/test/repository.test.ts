import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { Optional } from '@putnami/runtime';
import type postgres from 'postgres';
import { Column, type InferTable, Key, Migrator, Repository, RepositoryError, Table } from '../src/index';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

const DB_NAME = 'sql-test';
const dockerAvailable = await isDockerAvailable();

/**
 * Example table definition for testing
 */
const TestAssetsTable = Table(
  'test_assets',
  {
    id: Key(String),
    name: Column(String),
    symbol: Column(Optional(String)),
    currentPrice: Column(Number, { columnName: 'current_price' }),
    currency: Column(String),
  },
  { db: DB_NAME },
);

type TestAsset = InferTable<typeof TestAssetsTable>;

/**
 * Test repository
 */
class TestAssetRepository extends Repository<typeof TestAssetsTable> {
  constructor() {
    super(TestAssetsTable);
  }
}

let repository: TestAssetRepository;
let sql: Awaited<ReturnType<typeof import('../src/factory').database>>;

describe.skipIf(!dockerAvailable)('Repository', () => {
  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);

    repository = new TestAssetRepository();
    sql = await repository.conn();

    // A prior test run may have dropped the table while leaving its
    // migration row behind (afterAll only drops the table). Clear
    // both so the Migrator below actually re-creates the table.
    await sql`CREATE SCHEMA IF NOT EXISTS migration`;
    await sql`CREATE TABLE IF NOT EXISTS migration.migrations (
      id TEXT PRIMARY KEY, db_name TEXT NOT NULL, name TEXT NOT NULL,
      hash TEXT NOT NULL, executed_at TEXT NOT NULL, execution_time_ms INTEGER NOT NULL,
      success INTEGER NOT NULL, error_message TEXT, down_sql TEXT, down_hash TEXT
    )`;
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DB_NAME}`;

    const migrator = new Migrator(DB_NAME, [
      {
        name: 'sql-test/create-test-table',
        sql: `CREATE TABLE IF NOT EXISTS test_assets (
          id TEXT PRIMARY KEY,
          name TEXT,
          symbol TEXT,
          current_price NUMERIC,
          currency TEXT
        );`,
      },
    ]);
    await migrator.up();

    // Verify table exists
    sql = await repository.conn();
    const tableCheck = await sql`
      SELECT EXISTS (
        SELECT FROM information_schema.tables
        WHERE table_name = 'test_assets'
      );
    `;

    if (!tableCheck[0].exists) {
      throw new Error('Table test_assets was not created by migrations');
    }

    // Clean up before tests
    try {
      await sql`DELETE FROM test_assets`;
    } catch (_e) {
      // Ignore if table is empty or doesn't exist
    }
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    if (sql) {
      await sql`DELETE FROM test_assets`;
    }
  });

  describe('Metadata', () => {
    it('should have correct table name', () => {
      expect((repository as any).helper.tableName).toEqual('test_assets');
    });

    it('should have correct column names', () => {
      const columnNames = (repository as any).helper.columnNames;
      expect(columnNames).toContain('id');
      expect(columnNames).toContain('name');
      expect(columnNames).toContain('symbol');
      expect(columnNames).toContain('current_price');
      expect(columnNames).toContain('currency');
      expect(columnNames.length).toBe(5);
    });
  });

  describe('save', () => {
    it('should save a new entity', async () => {
      const entity: TestAsset = {
        id: 'TEST-1',
        name: 'Test Asset 1',
        symbol: 'TEST1',
        currentPrice: 100.5,
        currency: 'USD',
      };

      const saved = await repository.save(entity);

      expect(saved.id).toBe('TEST-1');
      expect(saved.name).toBe('Test Asset 1');
      expect(saved.currentPrice).toBe(100.5);
      expect(saved.currency).toBe('USD');
    });

    it('should update an existing entity', async () => {
      const entity: TestAsset = {
        id: 'TEST-2',
        name: 'Test Asset 2',
        currentPrice: 200.0,
        currency: 'USD',
      };

      await repository.save(entity);

      const updated = await repository.save({
        id: 'TEST-2',
        name: 'Updated Asset 2',
        currentPrice: 250.0,
        currency: 'EUR',
      });

      expect(updated.name).toBe('Updated Asset 2');
      expect(updated.currentPrice).toBe(250.0);
      expect(updated.currency).toBe('EUR');
    });

    it('should throw RepositoryError when saving empty entity', async () => {
      await expect(repository.save({} as any)).rejects.toThrow(RepositoryError);
      try {
        await repository.save({} as any);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('EMPTY_ENTITY');
        expect((error as RepositoryError).message).toContain('Cannot save an empty entity');
      }
    });

    it('should throw RepositoryError when saving entity without primary key', async () => {
      await expect(repository.save({ name: 'Test' } as any)).rejects.toThrow(RepositoryError);
      try {
        await repository.save({ name: 'Test' } as any);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('MISSING_PRIMARY_KEY');
        expect((error as RepositoryError).message).toContain('Cannot save an entity without primary key');
      }
    });

    it('should throw RepositoryError with cause when database operation fails', async () => {
      // Create a table constraint that will cause a failure
      await sql`
        ALTER TABLE test_assets
        ADD CONSTRAINT test_assets_name_unique UNIQUE (name)
      `.catch(() => {
        // Constraint might already exist, ignore
      });

      // Save first entity
      await repository.save({
        id: 'TEST-ERROR-1',
        name: 'Unique Name',
        currentPrice: 100,
        currency: 'USD',
      });

      // Try to save another entity with same name (should fail due to constraint)
      try {
        await repository.save({
          id: 'TEST-ERROR-2',
          name: 'Unique Name', // Duplicate name
          currentPrice: 200,
          currency: 'USD',
        });
        // If we get here, the test should fail
        expect(true).toBe(false);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        // A unique-constraint violation is mapped to a typed code with the
        // postgres metadata preserved, while pg detail stays off message.
        expect((error as RepositoryError).code).toBe('UNIQUE_VIOLATION');
        expect((error as RepositoryError).pgCode).toBe('23505');
        expect((error as RepositoryError).constraint).toBe('test_assets_name_unique');
        expect((error as RepositoryError).message).toBe('Failed to save entity');
        expect((error as RepositoryError).message).not.toContain('Unique Name');
        expect((error as RepositoryError).cause).toBeDefined();
      }

      // Clean up constraint
      await sql`
        ALTER TABLE test_assets
        DROP CONSTRAINT IF EXISTS test_assets_name_unique
      `.catch(() => {
        // Ignore if constraint doesn't exist
      });
    });

    it('should throw VALIDATION_ERROR with strict mode when required field is missing', async () => {
      try {
        await repository.save({ id: 'TEST-STRICT-1', currentPrice: 100 } as any, { strict: true });
        expect(true).toBe(false);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('VALIDATION_ERROR');
        expect((error as RepositoryError).message).toContain('required');
      }
    });

    it('should allow partial save without strict mode (default)', async () => {
      // First create the entity
      await repository.save({
        id: 'TEST-PARTIAL-1',
        name: 'Original Name',
        currentPrice: 100,
        currency: 'USD',
      });

      // Partial update without strict — should work (only validate provided fields)
      const updated = await repository.save({ id: 'TEST-PARTIAL-1', name: 'Updated Name' });
      expect(updated.name).toBe('Updated Name');
    });

    it('should throw VALIDATION_ERROR when field has wrong type', async () => {
      try {
        await repository.save({
          id: 'TEST-VALIDATE-1',
          name: 'Valid Name',
          currentPrice: 'not-a-number' as unknown as number,
          currency: 'USD',
        });
        expect(true).toBe(false);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('VALIDATION_ERROR');
        expect((error as RepositoryError).message).toContain('Validation failed');
        expect((error as RepositoryError).message).toContain('number');
      }
    });
  });

  describe('get', () => {
    it('should get entity by primary key', async () => {
      const entity: TestAsset = {
        id: 'TEST-GET-1',
        name: 'Get Test 1',
        currentPrice: 150.0,
        currency: 'USD',
      };

      await repository.save(entity);
      const found = await repository.get({ id: 'TEST-GET-1' });

      expect(found).toBeDefined();
      expect(found?.id).toBe('TEST-GET-1');
      expect(found?.name).toBe('Get Test 1');
    });

    it('should return undefined for non-existent entity', async () => {
      const found = await repository.get({ id: 'NON-EXISTENT' });
      expect(found).toBeUndefined();
    });
  });

  describe('findOne', () => {
    it('should find entity by filter', async () => {
      const entity: TestAsset = {
        id: 'TEST-FIND-1',
        name: 'Find Test 1',
        symbol: 'FIND1',
        currentPrice: 175.0,
        currency: 'USD',
      };

      await repository.save(entity);
      const found = await repository.findOne({ symbol: 'FIND1' });

      expect(found).toBeDefined();
      expect(found?.id).toBe('TEST-FIND-1');
    });

    it('should return undefined when no match', async () => {
      const found = await repository.findOne({ symbol: 'NON-EXISTENT' });
      expect(found).toBeUndefined();
    });
  });

  describe('find', () => {
    it('should find all entities when no filters', async () => {
      await repository.save({
        id: 'TEST-FIND-ALL-1',
        name: 'Find All 1',
        currentPrice: 100,
        currency: 'USD',
      });

      const results = await repository.find({});
      expect(results.length).toBeGreaterThan(0);
    });

    it('should find entities with filters', async () => {
      await repository.save({
        id: 'TEST-FILTER-1',
        name: 'Filter Test 1',
        currentPrice: 200,
        currency: 'USD',
      });

      const results = await repository.find({ currency: 'USD' });
      expect(results.length).toBeGreaterThan(0);
      expect(results.every((r) => r.currency === 'USD')).toBe(true);
    });

    it('should respect limit option', async () => {
      for (let i = 0; i < 5; i++) {
        await repository.save({
          id: `TEST-LIMIT-${i}`,
          name: `Limit Test ${i}`,
          currentPrice: 100 + i,
          currency: 'USD',
        });
      }

      const results = await repository.find({}, { limit: 2 });
      expect(results.length).toBeLessThanOrEqual(2);
    });

    it('should respect orderBy option with column name', async () => {
      await repository.save({
        id: 'TEST-ORDER-1',
        name: 'Order Test 1',
        currentPrice: 100,
        currency: 'USD',
      });

      await repository.save({
        id: 'TEST-ORDER-2',
        name: 'Order Test 2',
        currentPrice: 200,
        currency: 'USD',
      });

      const results = await repository.find({ currency: 'USD' }, { orderBy: 'currentPrice DESC' });
      if (results.length >= 2) {
        expect(results[0].currentPrice).toBeGreaterThanOrEqual(results[1].currentPrice);
      }
    });

    it('should map property name to column name in orderBy', async () => {
      await repository.save({
        id: 'TEST-ORDER-PROP-1',
        name: 'Order Prop Test 1',
        currentPrice: 50,
        currency: 'USD',
      });

      await repository.save({
        id: 'TEST-ORDER-PROP-2',
        name: 'Order Prop Test 2',
        currentPrice: 150,
        currency: 'USD',
      });

      const results = await repository.find({ currency: 'USD' }, { orderBy: 'currentPrice DESC' });

      const testResults = results.filter((r) => r.id.startsWith('TEST-ORDER-PROP'));
      if (testResults.length >= 2) {
        expect(testResults[0].currentPrice).toBeGreaterThanOrEqual(testResults[1].currentPrice);
      }
    });

    it('should handle orderBy with ASC direction', async () => {
      await repository.save({
        id: 'TEST-ORDER-ASC-1',
        name: 'Order ASC Test 1',
        currentPrice: 200,
        currency: 'USD',
      });

      await repository.save({
        id: 'TEST-ORDER-ASC-2',
        name: 'Order ASC Test 2',
        currentPrice: 100,
        currency: 'USD',
      });

      const results = await repository.find({ currency: 'USD' }, { orderBy: 'currentPrice ASC' });
      const testResults = results.filter((r) => r.id.startsWith('TEST-ORDER-ASC'));
      if (testResults.length >= 2) {
        expect(testResults[0].currentPrice).toBeLessThanOrEqual(testResults[1].currentPrice);
      }
    });

    it('should support multi-column orderBy', async () => {
      await repository.save({
        id: 'TEST-MULTI-ORDER-1',
        name: 'Alpha',
        currentPrice: 200,
        currency: 'GBP',
      });

      await repository.save({
        id: 'TEST-MULTI-ORDER-2',
        name: 'Beta',
        currentPrice: 100,
        currency: 'GBP',
      });

      await repository.save({
        id: 'TEST-MULTI-ORDER-3',
        name: 'Gamma',
        currentPrice: 100,
        currency: 'GBP',
      });

      const results = await repository.find({ currency: 'GBP' }, { orderBy: 'currentPrice ASC, name DESC' });

      const testResults = results.filter((r) => r.id.startsWith('TEST-MULTI-ORDER'));
      expect(testResults.length).toBe(3);
      // First two should have price 100 (sorted ASC), then price 200
      expect(testResults[0].currentPrice).toBe(100);
      expect(testResults[1].currentPrice).toBe(100);
      expect(testResults[2].currentPrice).toBe(200);
      // Among price 100, name should be DESC: Gamma before Beta
      expect(testResults[0].name).toBe('Gamma');
      expect(testResults[1].name).toBe('Beta');
    });

    it('should prevent SQL injection by rejecting unknown properties in orderBy', async () => {
      const maliciousOrderBy = 'currentPrice; DROP TABLE test_assets; --';

      await expect(repository.find({ currency: 'USD' }, { orderBy: maliciousOrderBy })).rejects.toThrow(
        /Unknown property/,
      );

      const tableCheck = await sql`
        SELECT EXISTS (
          SELECT FROM information_schema.tables
          WHERE table_name = 'test_assets'
        );
      `;
      expect(tableCheck[0]['exists']).toBe(true);
    });

    it('should reject unknown property names in orderBy', async () => {
      await expect(repository.find({ currency: 'USD' }, { orderBy: 'unknownProp DESC' })).rejects.toThrow(
        /Unknown property "unknownProp"/,
      );
    });

    it('should prevent SQL injection via unknown filter keys in WHERE', async () => {
      const maliciousFilter = { 'name" = name OR "1"="1': 'x' } as unknown as Partial<TestAsset>;

      await expect(repository.find(maliciousFilter)).rejects.toThrow(/Unknown property/);
      // delete() resolves primary keys first: a non-key (here, injected) key
      // never reaches SQL, so it is rejected as a missing-key delete.
      await expect(repository.delete(maliciousFilter)).rejects.toThrow(/without a primary key/);

      // The query never executes, so the table is untouched.
      const tableCheck = await sql`
        SELECT EXISTS (
          SELECT FROM information_schema.tables
          WHERE table_name = 'test_assets'
        );
      `;
      expect(tableCheck[0]['exists']).toBe(true);
    });

    it('should ignore undefined filter values', async () => {
      const results = await repository.find({ currency: 'USD', symbol: undefined });
      expect(Array.isArray(results)).toBe(true);
    });
  });

  describe('exists', () => {
    it('should return true for existing entity', async () => {
      const entity: TestAsset = {
        id: 'TEST-EXISTS-1',
        name: 'Exists Test 1',
        currentPrice: 100,
        currency: 'USD',
      };

      await repository.save(entity);
      const exists = await repository.exists({ id: 'TEST-EXISTS-1' });
      expect(exists).toBe(true);
    });

    it('should return false for non-existent entity', async () => {
      const exists = await repository.exists({ id: 'NON-EXISTENT-EXISTS' });
      expect(exists).toBe(false);
    });

    it('should return false when no keys provided', async () => {
      const exists = await repository.exists({});
      expect(exists).toBe(false);
    });
  });

  describe('delete', () => {
    it('should delete existing entity', async () => {
      const entity: TestAsset = {
        id: 'TEST-DELETE-1',
        name: 'Delete Test 1',
        currentPrice: 100,
        currency: 'USD',
      };

      await repository.save(entity);
      const result = await repository.delete({ id: 'TEST-DELETE-1' });

      expect(result.success).toBe(true);
      expect(result.item).toBeDefined();
      expect(result.item?.id).toBe('TEST-DELETE-1');

      // Verify it's deleted
      const found = await repository.get({ id: 'TEST-DELETE-1' });
      expect(found).toBeUndefined();
    });

    it('should return success false for non-existent entity', async () => {
      const result = await repository.delete({ id: 'NON-EXISTENT-DELETE' });
      expect(result.success).toBe(false);
      expect(result.item).toBeUndefined();
    });

    it('should throw when no primary key provided (symmetric with deleteMany)', async () => {
      await expect(repository.delete({})).rejects.toThrow(RepositoryError);
      try {
        await repository.delete({});
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('DELETE_WITHOUT_FILTERS');
      }
    });

    it('should throw when only non-key fields provided (deletes by primary key only)', async () => {
      // `currency` is not a key column; delete must not run an unkeyed delete.
      await expect(repository.delete({ currency: 'USD' } as Partial<TestAsset>)).rejects.toThrow(
        /without a primary key/,
      );
    });

    it('should match only on key columns, ignoring extra non-key fields', async () => {
      await repository.save({ id: 'TEST-DELETE-KEYONLY', name: 'Keep', currentPrice: 1, currency: 'USD' });

      // A wrong non-key field must not prevent the key-based delete.
      const result = await repository.delete({ id: 'TEST-DELETE-KEYONLY', currency: 'NOPE' } as Partial<TestAsset>);
      expect(result.success).toBe(true);
      expect(result.item?.id).toBe('TEST-DELETE-KEYONLY');
    });
  });

  describe('conn', () => {
    it('should return database connection', async () => {
      const connection = await repository.conn();
      expect(connection).toBeDefined();

      const result = await connection`SELECT 1 as test`;
      expect(result[0].test).toBe(1);
    });
  });

  describe('saveMany', () => {
    it('should save multiple entities', async () => {
      const entities = [
        {
          id: 'BULK-1',
          name: 'Bulk Test 1',
          currentPrice: 100,
          currency: 'USD',
        },
        {
          id: 'BULK-2',
          name: 'Bulk Test 2',
          currentPrice: 200,
          currency: 'USD',
        },
        {
          id: 'BULK-3',
          name: 'Bulk Test 3',
          currentPrice: 300,
          currency: 'EUR',
        },
      ];

      const saved = await repository.saveMany(entities);
      expect(saved).toHaveLength(3);
      expect(saved[0].id).toBe('BULK-1');
      expect(saved[1].id).toBe('BULK-2');
      expect(saved[2].id).toBe('BULK-3');
    });

    it('should throw error when saving empty array', async () => {
      await expect(repository.saveMany([])).rejects.toThrow(RepositoryError);
      try {
        await repository.saveMany([]);
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('EMPTY_ENTITIES');
      }
    });
  });

  describe('deleteMany', () => {
    it('should delete multiple entities matching filters', async () => {
      await repository.saveMany([
        {
          id: 'DELETE-MANY-1',
          name: 'Delete Many 1',
          currentPrice: 100,
          currency: 'USD',
        },
        {
          id: 'DELETE-MANY-2',
          name: 'Delete Many 2',
          currentPrice: 200,
          currency: 'USD',
        },
        {
          id: 'DELETE-MANY-3',
          name: 'Delete Many 3',
          currentPrice: 300,
          currency: 'EUR',
        },
      ]);

      const deletedCount = await repository.deleteMany({ currency: 'USD' });
      expect(deletedCount).toBeGreaterThanOrEqual(2);

      const remaining = await repository.find({ currency: 'USD' });
      const deletedEntities = remaining.filter((r) => r.id.startsWith('DELETE-MANY'));
      expect(deletedEntities.length).toBe(0);
    });

    it('should throw error when deleting without filters', async () => {
      await expect(repository.deleteMany({})).rejects.toThrow(RepositoryError);
      try {
        await repository.deleteMany({});
      } catch (error) {
        expect(error).toBeInstanceOf(RepositoryError);
        expect((error as RepositoryError).code).toBe('DELETE_WITHOUT_FILTERS');
      }
    });
  });

  describe('Pagination (offset)', () => {
    it('should support offset parameter', async () => {
      const entities = Array.from({ length: 5 }, (_, i) => ({
        id: `PAGINATION-${i}`,
        name: `Pagination Test ${i}`,
        currentPrice: 100 + i * 10,
        currency: 'USD',
      }));
      await repository.saveMany(entities);

      const firstPage = await repository.find({ currency: 'USD' }, { limit: 2, orderBy: 'id ASC' });
      expect(firstPage.length).toBeLessThanOrEqual(2);

      const secondPage = await repository.find({ currency: 'USD' }, { limit: 2, offset: 2, orderBy: 'id ASC' });
      expect(secondPage.length).toBeLessThanOrEqual(2);

      if (firstPage.length > 0 && secondPage.length > 0) {
        expect(firstPage[0].id).not.toBe(secondPage[0].id);
      }
    });

    it('should handle offset 0', async () => {
      const results = await repository.find({ currency: 'USD' }, { limit: 10, offset: 0 });
      expect(Array.isArray(results)).toBe(true);
    });
  });

  describe('Query operators', () => {
    beforeEach(async () => {
      await repository.saveMany([
        {
          id: 'OP-TEST-1',
          name: 'Operator Test 1',
          currentPrice: 100,
          currency: 'USD',
        },
        {
          id: 'OP-TEST-2',
          name: 'Operator Test 2',
          currentPrice: 200,
          currency: 'USD',
        },
        {
          id: 'OP-TEST-3',
          name: 'Operator Test 3',
          currentPrice: 300,
          currency: 'EUR',
        },
      ]);
    });

    it('should support in operator', async () => {
      const results = await repository.find({
        id: { in: ['OP-TEST-1', 'OP-TEST-2'] },
      });
      expect(results.length).toBeGreaterThanOrEqual(2);
      expect(results.some((r) => r.id === 'OP-TEST-1')).toBe(true);
      expect(results.some((r) => r.id === 'OP-TEST-2')).toBe(true);
    });

    it('should support notIn operator', async () => {
      const results = await repository.find({
        id: { notIn: ['OP-TEST-1', 'OP-TEST-2'] },
      });
      const hasExcluded = results.some((r) => r.id === 'OP-TEST-1' || r.id === 'OP-TEST-2');
      expect(hasExcluded).toBe(false);
    });

    it('should treat empty notIn as no restriction, including rows where the column is null', async () => {
      const results = await repository.find({
        symbol: { notIn: [] },
      });
      const ids = results.map((r) => r.id);
      expect(ids).toContain('OP-TEST-1');
      expect(ids).toContain('OP-TEST-2');
      expect(ids).toContain('OP-TEST-3');
    });

    it('should support gt operator', async () => {
      const results = await repository.find({
        currentPrice: { gt: 150 },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currentPrice).toBeGreaterThan(150);
      }
    });

    it('should support lt operator', async () => {
      const results = await repository.find({
        currentPrice: { lt: 250 },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currentPrice).toBeLessThan(250);
      }
    });

    it('should support gte operator', async () => {
      const results = await repository.find({
        currentPrice: { gte: 200 },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currentPrice).toBeGreaterThanOrEqual(200);
      }
    });

    it('should support lte operator', async () => {
      const results = await repository.find({
        currentPrice: { lte: 200 },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currentPrice).toBeLessThanOrEqual(200);
      }
    });

    it('should support not operator', async () => {
      const results = await repository.find({
        currency: { not: 'USD' },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currency).not.toBe('USD');
      }
    });

    it('should support like operator', async () => {
      const results = await repository.find({
        name: { like: '%Operator Test%' },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.name).toContain('Operator Test');
      }
    });

    it('should support ilike operator (case insensitive)', async () => {
      const results = await repository.find({
        name: { ilike: '%operator test%' },
      });
      expect(results.length).toBeGreaterThan(0);
    });

    it('should support isNull operator', async () => {
      await repository.save({
        id: 'NULL-TEST',
        name: 'Null Test',
        currentPrice: 100,
        currency: 'USD',
        symbol: undefined,
      });

      const results = await repository.find({
        symbol: { isNull: true },
      });
      expect(Array.isArray(results)).toBe(true);
    });

    it('should support isNotNull operator', async () => {
      const results = await repository.find({
        name: { isNotNull: true },
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.name).toBeDefined();
      }
    });

    it('should support $or conditions', async () => {
      const results = await repository.find({
        $or: [{ id: 'OP-TEST-1' }, { id: 'OP-TEST-3' }],
      });
      const ids = results.map((r) => r.id);
      expect(ids).toContain('OP-TEST-1');
      expect(ids).toContain('OP-TEST-3');
      expect(ids).not.toContain('OP-TEST-2');
    });

    it('should combine $or with a regular filter using AND', async () => {
      const results = await repository.find({
        currency: 'USD',
        $or: [{ id: 'OP-TEST-1' }, { id: 'OP-TEST-3' }],
      });
      // OP-TEST-3 is EUR, so the currency=USD AND (...) leaves only OP-TEST-1.
      const ids = results.map((r) => r.id);
      expect(ids).toContain('OP-TEST-1');
      expect(ids).not.toContain('OP-TEST-3');
    });

    it('should support $and conditions', async () => {
      const results = await repository.find({
        $and: [{ currency: 'USD' }, { currentPrice: { gte: 200 } }],
      });
      expect(results.length).toBeGreaterThan(0);
      for (const r of results) {
        expect(r.currency).toBe('USD');
        expect(r.currentPrice).toBeGreaterThanOrEqual(200);
      }
    });
  });
});

/**
 * NUMERIC precision: the driver reads NUMERIC back as a lossless string. A
 * column declared as `String` must keep the exact decimal; a column declared as
 * `Number` opts into JS-number semantics.
 */
const MoneyTable = Table(
  'money_items',
  {
    id: Key(String),
    // Exact decimal preserved end-to-end.
    amount: Column(String),
    // Numeric column — coerced back to a JS number.
    rate: Column(Number),
  },
  { db: DB_NAME },
);

class MoneyRepository extends Repository<typeof MoneyTable> {
  constructor() {
    super(MoneyTable);
  }
}

describe.skipIf(!dockerAvailable)('NUMERIC precision', () => {
  let moneyRepo: MoneyRepository;
  let moneySql: Awaited<ReturnType<typeof import('../src/factory').database>>;

  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);
    moneyRepo = new MoneyRepository();
    moneySql = await moneyRepo.conn();
    await moneySql`
      CREATE TABLE IF NOT EXISTS money_items (
        id TEXT PRIMARY KEY,
        amount NUMERIC,
        rate NUMERIC
      )
    `;
    await moneySql`DELETE FROM money_items`;
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    if (moneySql) {
      await moneySql`DROP TABLE IF EXISTS money_items`;
    }
  });

  it('preserves a high-precision decimal for a String-typed NUMERIC column', async () => {
    // A value with more significant digits than a float64 can represent exactly.
    const exact = '12345678901234567890.12345678901234567890';
    await moneyRepo.save({ id: 'M1', amount: exact, rate: 1.5 });

    const found = await moneyRepo.get({ id: 'M1' });
    expect(found?.amount).toBe(exact);
    expect(typeof found?.amount).toBe('string');
  });

  it('coerces a Number-typed NUMERIC column back to a JS number', async () => {
    await moneyRepo.save({ id: 'M2', amount: '0', rate: 100.5 });

    const found = await moneyRepo.get({ id: 'M2' });
    expect(found?.rate).toBe(100.5);
    expect(typeof found?.rate).toBe('number');
  });
});

/**
 * A DB-free postgres.js tag that records the final find() query's LIMIT value.
 * The repository's private ceiling is pinned directly so this verifies the
 * query-building behaviour without competing for the process-global test pool.
 */
function findSql(limits: number[]): postgres.Sql {
  const sql = ((strings: TemplateStringsArray | string, ...values: unknown[]) => {
    if (typeof strings === 'string') {
      return { kind: 'identifier' };
    }
    if (strings.join('').includes('SELECT *')) {
      const limit = values.find((value): value is number => typeof value === 'number');
      if (limit === undefined) {
        throw new Error('find() query did not include a LIMIT value');
      }
      limits.push(limit);
      return Promise.resolve([]);
    }
    return { kind: 'fragment' };
  }) as unknown as postgres.Sql;
  return sql;
}

function findLimit(requested: number): Promise<number> {
  const limits: number[] = [];
  const repo = new TestAssetRepository();
  const sql = findSql(limits);

  (repo as unknown as { conn: () => Promise<postgres.Sql> }).conn = async () => sql;
  (repo as unknown as { _maxRowLimit: number })._maxRowLimit = 3;

  return repo.find({}, { limit: requested }).then(() => {
    const limit = limits[0];
    if (limit === undefined) {
      throw new Error('find() did not issue a query');
    }
    return limit;
  });
}

describe('find() maxRowLimit clamp', () => {
  it('clamps an over-large explicit limit to the configured ceiling', async () => {
    expect(await findLimit(100_000_000)).toBe(3);
  });

  it('still honors a small explicit limit below the ceiling', async () => {
    expect(await findLimit(2)).toBe(2);
  });
});
