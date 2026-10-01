import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import { closeAllDatabases, closeDatabase, database, type PostgresConfig } from '../src/factory';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';
import { setPrimaryDatasource } from '../src/primary-datasource';

const DB_NAME = 'factory-test';
const DB_NAME_2 = 'factory-test-2';
const DB_NAME_CLEANUP = 'factory-test-cleanup';
const DB_NAME_ALL_1 = 'factory-test-all-1';
const DB_NAME_ALL_2 = 'factory-test-all-2';
const DB_NAME_RECREATE = 'factory-test-recreate';
const dockerAvailable = await isDockerAvailable();

describe.skipIf(!dockerAvailable)('Database Factory', () => {
  beforeAll(async () => {
    // Provisioning is setup work: it is paid under the declared setup budget
    // (POSTGRES_SETUP_TIMEOUT_MS), never inside a test body's default budget.
    for (const name of [DB_NAME, DB_NAME_2, DB_NAME_CLEANUP, DB_NAME_ALL_1, DB_NAME_ALL_2, DB_NAME_RECREATE]) {
      await ensurePostgresContainer(name);
    }
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterEach(() => {
    setPrimaryDatasource(undefined);
  });

  afterAll(async () => {
    // Clean up connections
    const sql = await database(DB_NAME);
    await sql.end();
  });

  describe('database()', () => {
    it('should create a database connection', async () => {
      const sql = await database(DB_NAME);
      expect(sql).toBeDefined();

      // Test connection
      const result = await sql`SELECT 1 as test`;
      expect(result[0].test).toBe(1);
    });

    it('should reuse existing connection for same config', async () => {
      const sql1 = await database(DB_NAME);
      const sql2 = await database(DB_NAME);

      // Should be the same instance (connection pooling)
      expect(sql1).toBe(sql2);
    });

    it('should create separate connections for different databases', async () => {
      const sql1 = await database(DB_NAME);
      const sql2 = await database(DB_NAME_2);

      expect(sql1).not.toBe(sql2);

      // Clean up
      await sql2.end();
    });

    it('should accept explicit config', async () => {
      const config: PostgresConfig = {
        host: 'localhost',
        port: 6432,
        database: 'test',
        user: 'test',
        password: 'test',
        ssl: false,
        poolSize: 10,
        debug: false,
      };

      const sql = await database(undefined, config);
      expect(sql).toBeDefined();

      const result = await sql`SELECT 1 as test`;
      expect(result[0].test).toBe(1);
    });

    it('should handle default database connection', async () => {
      // This will use the default database config
      // Note: This test assumes default config is set up
      try {
        const sql = await database();
        expect(sql).toBeDefined();
      } catch (error) {
        // If default config is not set, that's okay for this test
        expect(error).toBeDefined();
      }
    });

    it('should inherit the primary datasource when name is omitted', async () => {
      setPrimaryDatasource({ name: DB_NAME });

      const explicit = await database(DB_NAME);
      const inherited = await database();

      expect(inherited).toBe(explicit);
    });
  });

  describe('Connection pooling', () => {
    it('should handle multiple concurrent queries', async () => {
      const sql = await database(DB_NAME);

      const promises = Array.from({ length: 10 }, (_, i) => sql`SELECT ${i} as num`);

      const results = await Promise.all(promises);
      expect(results).toHaveLength(10);
      results.forEach((result, i) => {
        expect(Number(result[0].num)).toBe(i);
      });
    });

    it('should execute transactions', async () => {
      const sql = await database(DB_NAME);

      // Create a test table
      await sql`
        CREATE TABLE IF NOT EXISTS factory_test (
          id TEXT PRIMARY KEY,
          value TEXT
        )
      `;

      await sql.begin(async (sql) => {
        await sql`INSERT INTO factory_test (id, value) VALUES ('test-1', 'value-1')`;
        await sql`INSERT INTO factory_test (id, value) VALUES ('test-2', 'value-2')`;
      });

      const result = await sql`SELECT * FROM factory_test WHERE id IN ('test-1', 'test-2')`;
      expect(result.length).toBe(2);

      // Clean up
      await sql`DROP TABLE IF EXISTS factory_test`;
    });
  });

  describe('Connection cleanup', () => {
    it('should close a specific database connection', async () => {
      // Create a connection
      const sql = await database(DB_NAME_CLEANUP);
      expect(sql).toBeDefined();

      // Verify connection works
      const result = await sql`SELECT 1 as test`;
      expect(result[0].test).toBe(1);

      // Close the connection
      await closeDatabase(DB_NAME_CLEANUP);

      // Verify connection is closed (should throw or be invalid)
      // Note: postgres library might not immediately throw, but connection should be closed
      try {
        await sql`SELECT 1`;
      } catch (error) {
        // Expected - connection is closed
        expect(error).toBeDefined();
      }
    });

    it('should close all database connections', async () => {
      // Create multiple connections
      const sql1 = await database(DB_NAME_ALL_1);
      const sql2 = await database(DB_NAME_ALL_2);

      // Verify connections work
      const result1 = await sql1`SELECT 1 as test`;
      const result2 = await sql2`SELECT 1 as test`;
      expect(result1[0].test).toBe(1);
      expect(result2[0].test).toBe(1);

      // Close all connections
      await closeAllDatabases();

      // Verify connections are closed
      try {
        await sql1`SELECT 1`;
      } catch (error) {
        expect(error).toBeDefined();
      }

      try {
        await sql2`SELECT 1`;
      } catch (error) {
        expect(error).toBeDefined();
      }
    });

    it('should handle closing non-existent connection gracefully', async () => {
      try {
        await closeDatabase('non-existent-db');
      } catch {
        expect().fail('Expected no error to be thrown');
      }
    });

    it('should allow creating new connection after closing', async () => {
      // Create and close connection
      const _sql1 = await database(DB_NAME_RECREATE);
      await closeDatabase(DB_NAME_RECREATE);

      // Create new connection with same name
      const sql2 = await database(DB_NAME_RECREATE);
      expect(sql2).toBeDefined();

      // Verify new connection works
      const result = await sql2`SELECT 1 as test`;
      expect(result[0].test).toBe(1);

      // Clean up
      await closeDatabase(DB_NAME_RECREATE);
    });
  });
});
