import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { SessionConfig } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { database } from '../src/factory';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';
import { DatabaseSessionStore } from '../src/session/database.store';

const DB_NAME = 'session-test';
const dockerAvailable = await isDockerAvailable();

describe.skipIf(!dockerAvailable)('DatabaseSessionStore', () => {
  let store: DatabaseSessionStore;
  let sql: Awaited<ReturnType<typeof database>>;

  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);
    store = new DatabaseSessionStore(DB_NAME);
    sql = await database(DB_NAME);

    // Ensure table exists
    await store.ensureTable();
    await sql`DELETE FROM sessions`;
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await sql`DELETE FROM sessions`;
  }, 10_000); // Increase timeout for cleanup

  describe('ensureTable', () => {
    it('should create sessions table', async () => {
      const newStore = new DatabaseSessionStore(DB_NAME);
      await newStore.ensureTable();

      const result = await sql`
        SELECT EXISTS (
          SELECT FROM information_schema.tables
          WHERE table_name = 'sessions'
        );
      `;
      expect(result[0].exists).toBe(true);
    });

    it('should create index on expires_at', async () => {
      const result = await sql`
        SELECT EXISTS (
          SELECT FROM pg_indexes
          WHERE indexname = 'idx_sessions_expires_at'
        );
      `;
      expect(result[0].exists).toBe(true);
    });
  });

  describe('setAllAsync / getAllAsync', () => {
    it('should store and retrieve session data', async () => {
      const sessionId = 'test-session-1';
      const sessionData = {
        userId: 'user-123',
        role: 'admin',
        preferences: { theme: 'dark' },
      };

      await store.setAllAsync(sessionId, sessionData);
      const retrieved = await store.getAllAsync(sessionId);

      expect(retrieved.userId).toBe('user-123');
      expect(retrieved.role).toBe('admin');
      expect(retrieved.preferences.theme).toBe('dark');
    });

    it('should update existing session', async () => {
      const sessionId = 'test-session-2';
      const initialData = { userId: 'user-123', count: 1 };
      const updatedData = { userId: 'user-123', count: 2 };

      await store.setAllAsync(sessionId, initialData);
      await store.setAllAsync(sessionId, updatedData);

      const retrieved = await store.getAllAsync(sessionId);
      expect(retrieved.count).toBe(2);
    });

    it('should return empty object for non-existent session', async () => {
      const retrieved = await store.getAllAsync('non-existent-session');
      expect(retrieved).toEqual({});
    });

    it('should extend expiry on get', async () => {
      const sessionId = 'test-session-3';
      const sessionData = { userId: 'user-123' };

      await store.setAllAsync(sessionId, sessionData);

      // Get initial expiry (postgres.camel transforms expires_at to expiresAt)
      const initial = await sql<{ expiresAt: Date }[]>`
        SELECT expires_at FROM sessions WHERE session_id = ${sessionId}
      `;
      const initialExpiry =
        initial[0]?.expiresAt instanceof Date ? initial[0].expiresAt : new Date(initial[0].expiresAt);

      // Wait a bit
      await new Promise((resolve) => setTimeout(resolve, 100));

      // Get session (should extend expiry)
      await store.getAllAsync(sessionId);

      // Check new expiry
      const updated = await sql<{ expiresAt: Date }[]>`
        SELECT expires_at FROM sessions WHERE session_id = ${sessionId}
      `;
      const updatedExpiry =
        updated[0]?.expiresAt instanceof Date ? updated[0].expiresAt : new Date(updated[0].expiresAt);

      expect(updatedExpiry.getTime()).toBeGreaterThan(initialExpiry.getTime());
    });
  });

  describe('getAsync / setAllAsync', () => {
    it('should get single value from session', async () => {
      const sessionId = 'test-session-4';
      const sessionData = {
        userId: 'user-123',
        email: 'test@example.com',
      };

      await store.setAllAsync(sessionId, sessionData);
      const userId = await store.getAsync<string>(sessionId, 'userId');
      const email = await store.getAsync<string>(sessionId, 'email');

      expect(userId).toBe('user-123');
      expect(email).toBe('test@example.com');
    });

    it('should return undefined for non-existent key', async () => {
      const sessionId = 'test-session-5';
      await store.setAllAsync(sessionId, { userId: 'user-123' });

      const value = await store.getAsync<string>(sessionId, 'nonExistent');
      expect(value).toBeUndefined();
    });
  });

  describe('deleteAllAsync', () => {
    it('should delete session', async () => {
      const sessionId = 'test-session-6';
      const sessionData = { userId: 'user-123' };

      await store.setAllAsync(sessionId, sessionData);
      await store.deleteAllAsync(sessionId);

      const retrieved = await store.getAllAsync(sessionId);
      expect(retrieved).toEqual({});
    });
  });

  describe('existsAsync', () => {
    it('should return true for existing session', async () => {
      const sessionId = 'test-session-7';
      await store.setAllAsync(sessionId, { userId: 'user-123' });

      const exists = await store.existsAsync(sessionId);
      expect(exists).toBe(true);
    });

    it('should return false for non-existent session', async () => {
      const exists = await store.existsAsync('non-existent-session-7');
      expect(exists).toBe(false);
    });

    it('should return false for expired session', async () => {
      const sessionId = 'test-session-8';
      const sessionData = { userId: 'user-123' };

      await store.setAllAsync(sessionId, sessionData);

      // Manually expire the session
      await sql`
        UPDATE sessions
        SET expires_at = NOW() - INTERVAL '1 day'
        WHERE session_id = ${sessionId}
      `;

      const exists = await store.existsAsync(sessionId);
      expect(exists).toBe(false);
    });
  });

  describe('cleanup', () => {
    it('should delete expired sessions', async () => {
      const expiredSessionId = 'expired-session-1';
      const activeSessionId = 'active-session-1';

      // Create expired session
      await sql`
        INSERT INTO sessions (session_id, data, expires_at)
        VALUES (
          ${expiredSessionId},
          '{"userId": "user-123"}'::jsonb,
          NOW() - INTERVAL '1 day'
        )
      `;

      // Create active session
      await store.setAllAsync(activeSessionId, { userId: 'user-456' });

      const deletedCount = await store.cleanup();
      expect(deletedCount).toBeGreaterThan(0);

      // Verify expired session is gone
      const expiredExists = await store.existsAsync(expiredSessionId);
      expect(expiredExists).toBe(false);

      // Verify active session still exists
      const activeExists = await store.existsAsync(activeSessionId);
      expect(activeExists).toBe(true);
    });

    it('should return 0 when no expired sessions', async () => {
      // Clean all sessions first
      await sql`DELETE FROM sessions`;

      const deletedCount = await store.cleanup();
      expect(deletedCount).toBe(0);
    });
  });

  describe('TTL handling', () => {
    it('should set expiry based on TTL config', async () => {
      const sessionId = 'test-session-ttl';
      const config = useConfig(SessionConfig);
      const expectedTTL = config.ttl * 1000; // Convert to milliseconds

      await store.setAllAsync(sessionId, { userId: 'user-123' });

      // postgres.camel transforms expires_at to expiresAt and created_at to createdAt
      const result = await sql<{ expiresAt: Date; createdAt: Date }[]>`
        SELECT expires_at, created_at FROM sessions WHERE session_id = ${sessionId}
      `;
      const expiresAt = result[0]?.expiresAt instanceof Date ? result[0].expiresAt : new Date(result[0].expiresAt);
      const createdAt = result[0]?.createdAt instanceof Date ? result[0].createdAt : new Date(result[0].createdAt);
      const actualTTL = expiresAt.getTime() - createdAt.getTime();

      // Allow 1 second tolerance
      expect(Math.abs(actualTTL - expectedTTL)).toBeLessThan(1000);
    });
  });

  describe('Complex data types', () => {
    it('should handle arrays', async () => {
      const sessionId = 'test-session-array';
      const sessionData = {
        items: ['item-1', 'item-2', 'item-3'],
      };

      await store.setAllAsync(sessionId, sessionData);
      const retrieved = await store.getAllAsync<{ items: string[] }>(sessionId);

      expect(retrieved.items).toEqual(['item-1', 'item-2', 'item-3']);
    });

    it('should handle nested objects', async () => {
      const sessionId = 'test-session-nested';
      const sessionData = {
        user: {
          id: 'user-123',
          profile: {
            name: 'John Doe',
            settings: {
              theme: 'dark',
            },
          },
        },
      };

      await store.setAllAsync(sessionId, sessionData);
      const retrieved = await store.getAllAsync(sessionId);

      expect(retrieved.user.id).toBe('user-123');
      expect(retrieved.user.profile.name).toBe('John Doe');
      expect(retrieved.user.profile.settings.theme).toBe('dark');
    });
  });
});
