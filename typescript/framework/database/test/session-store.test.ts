import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import {
  application,
  config,
  deleteSessionAll,
  http,
  SessionConfig,
  setSession,
  useSession,
} from '@putnami/application';
import { runInContext, useConfig } from '@putnami/runtime';
import { QueryAbortError } from '../src/abort';
import { database } from '../src/factory';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';
import { PostgresConfig } from '../src/postgres/config';
import { DATABASE_PROTOCOL_VERSION, resolveDatabaseBinding, setDatabaseBindingOverride } from '../src/postgres/binding';
import { DatabaseSessionStore, SESSION_TABLE_SQL, sessionMigrations } from '../src/session/database.store';

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
      expect(result[0]['exists']).toBe(true);
    });

    it('should create index on expires_at', async () => {
      const result = await sql`
        SELECT EXISTS (
          SELECT FROM pg_indexes
          WHERE indexname = 'idx_sessions_expires_at'
        );
      `;
      expect(result[0]['exists']).toBe(true);
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

      expect(retrieved['userId']).toBe('user-123');
      expect(retrieved['role']).toBe('admin');
      expect((retrieved['preferences'] as { theme: string }).theme).toBe('dark');
    });

    it('writes nothing once the request is aborted, on a live database', async () => {
      const controller = new AbortController();
      controller.abort(new DOMException('Request Timeout', 'TimeoutError'));
      const outcome = await Promise.resolve(
        runInContext({ signal: controller.signal }, () => store.setAllAsync('aborted-session', { userId: 'user-123' })),
      ).catch((error: unknown) => error);
      expect(outcome).toBeInstanceOf(QueryAbortError);
      expect(await store.getAllAsync<Record<string, unknown>>('aborted-session')).toEqual({});
    });

    it('should update existing session', async () => {
      const sessionId = 'test-session-2';
      const initialData = { userId: 'user-123', count: 1 };
      const updatedData = { userId: 'user-123', count: 2 };

      await store.setAllAsync(sessionId, initialData);
      await store.setAllAsync(sessionId, updatedData);

      const retrieved = await store.getAllAsync(sessionId);
      expect(retrieved['count']).toBe(2);
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

      // setAllAsync computes expires_at from the host clock, so the bounds come from the host clock too.
      const before = Date.now();
      await store.setAllAsync(sessionId, { userId: 'user-123' });
      const after = Date.now();

      // postgres.camel transforms expires_at to expiresAt
      const result = await sql<{ expiresAt: Date }[]>`
        SELECT expires_at FROM sessions WHERE session_id = ${sessionId}
      `;
      const expiresAt = result[0]?.expiresAt instanceof Date ? result[0].expiresAt : new Date(result[0].expiresAt);

      expect(expiresAt.getTime()).toBeGreaterThanOrEqual(before + expectedTTL);
      expect(expiresAt.getTime()).toBeLessThanOrEqual(after + expectedTTL);
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

      const user = retrieved['user'] as { id: string; profile: { name: string; settings: { theme: string } } };
      expect(user.id).toBe('user-123');
      expect(user.profile.name).toBe('John Doe');
      expect(user.profile.settings.theme).toBe('dark');
    });

    it('returns every key as it was written, snake_case and nested included', async () => {
      const sessionId = 'test-session-keys';
      const sessionData = {
        oauth_pending: { state: 's-1', code_verifier: 'v-1', redirect_to: '/r/queue' },
        accessToken: 't-1',
        token_set: [{ expires_at: 1 }],
      };

      await store.setAllAsync(sessionId, sessionData);

      expect(await store.getAllAsync<typeof sessionData>(sessionId)).toEqual(sessionData);
      expect(await store.getAsync<typeof sessionData.oauth_pending>(sessionId, 'oauth_pending')).toEqual(
        sessionData.oauth_pending,
      );
    });
  });

  describe('behind the session API of an HTTP application', () => {
    function cookieOf(res: Response): string {
      return (res.headers.get('set-cookie') ?? '').split(';')[0];
    }

    /** The response, once it answered 200: a failure shows its status and the start of its body. */
    async function answered(res: Promise<Response>): Promise<Response> {
      const response = await res;
      if (response.status !== 200) {
        const body = (await response.text()).slice(0, 400);
        expect({ status: response.status, body }).toEqual({ status: 200, body: '' });
      }
      return response;
    }

    async function withServer(run: (base: string) => Promise<void>): Promise<void> {
      const hp = http({ port: 0 });
      hp.route('GET', '/read', () => ({ cart: useSession<string[]>('cart') ?? null }));
      hp.route('GET', '/write', () => {
        setSession('cart', ['item-1']);
        return { ok: true };
      });
      hp.route('GET', '/login', () => {
        setSession('oauth_pending', { state: 's-1', code_verifier: 'v-1' });
        return { ok: true };
      });
      hp.route('GET', '/callback', () => ({ pending: useSession('oauth_pending') ?? null }));
      hp.route('GET', '/forget', () => {
        deleteSessionAll();
        return { ok: true };
      });
      const app = application().use(hp).use(config());
      await app.start();
      try {
        await run(`http://localhost:${hp.getServer()?.port}`);
      } finally {
        await app.stop();
      }
    }

    it('keeps what one request wrote for the next request, in PostgreSQL', async () => {
      await sql`DELETE FROM sessions`;
      await withServer(async (base) => {
        const cookie = cookieOf(await answered(fetch(`${base}/write`)));
        expect(cookie.startsWith('session_id=')).toBe(true);
        const rows = await sql<{ data: { cart: string[] } }[]>`SELECT data FROM sessions`;
        expect(rows.map((row) => row.data)).toEqual([{ cart: ['item-1'] }]);

        const read = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
        expect(await read.json()).toEqual({ cart: ['item-1'] });
      });
    });

    it('reads back a snake_case key the previous request wrote, as an OAuth callback does', async () => {
      await sql`DELETE FROM sessions`;
      await withServer(async (base) => {
        const cookie = cookieOf(await answered(fetch(`${base}/login`)));
        const callback = await fetch(`${base}/callback`, { headers: { Cookie: cookie } });
        expect(await callback.json()).toEqual({ pending: { state: 's-1', code_verifier: 'v-1' } });
      });
    });

    it('stores no row for a visitor whose request only reads the session', async () => {
      await sql`DELETE FROM sessions`;
      await withServer(async (base) => {
        const read = await answered(fetch(`${base}/read`));
        expect(await read.json()).toEqual({ cart: null });
        const [{ count }] = await sql<{ count: number }[]>`SELECT count(*)::int AS count FROM sessions`;
        expect(count).toBe(0);
      });
    });

    it('deletes the row when a request empties the session', async () => {
      await sql`DELETE FROM sessions`;
      await withServer(async (base) => {
        const cookie = cookieOf(await fetch(`${base}/write`));
        await fetch(`${base}/forget`, { headers: { Cookie: cookie } });
        const [{ count }] = await sql<{ count: number }[]>`SELECT count(*)::int AS count FROM sessions`;
        expect(count).toBe(0);
      });
    });
  });

  describe('under a runtime role that may not create tables', () => {
    const RUNTIME = 'session-runtime';

    it('serves sessions from the table a migration created, without running DDL', async () => {
      await sql.unsafe(SESSION_TABLE_SQL);
      const [{ db, schema }] = await sql<{ db: string; schema: string }[]>`
        SELECT current_database() AS db, current_schema() AS schema
      `;
      await sql.unsafe(`DO $$ BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'session_runtime') THEN
          CREATE ROLE session_runtime LOGIN PASSWORD 'runtime';
        END IF;
      END $$`);
      await sql.unsafe(`GRANT CONNECT ON DATABASE "${db}" TO session_runtime`);
      await sql.unsafe(`GRANT USAGE ON SCHEMA "${schema}" TO session_runtime`);
      await sql.unsafe(`REVOKE CREATE ON SCHEMA "${schema}" FROM session_runtime`);
      await sql.unsafe('GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO session_runtime');
      const [{ canCreate }] = await sql<{ canCreate: boolean }[]>`
        SELECT has_schema_privilege('session_runtime', ${schema}, 'CREATE') AS "canCreate"
      `;
      expect(canCreate).toBe(false);

      const admin =
        resolveDatabaseBinding(DB_NAME)?.config ?? useConfig(PostgresConfig, { path: `database.${DB_NAME}` });
      const previous = setDatabaseBindingOverride(
        JSON.stringify({
          protocolVersion: DATABASE_PROTOCOL_VERSION,
          databases: {
            [RUNTIME]: {
              engine: 'postgres',
              schema,
              connection: {
                host: admin.host,
                port: admin.port,
                database: db,
                user: 'session_runtime',
                password: 'runtime',
                ssl: false,
              },
            },
          },
        }),
      );
      try {
        const runtime = new DatabaseSessionStore(RUNTIME);
        await runtime.ensureTable();
        await runtime.setAllAsync('runtime-session', { userId: 'user-9' });
        expect(await runtime.getAllAsync<Record<string, unknown>>('runtime-session')).toEqual({ userId: 'user-9' });
        await runtime.deleteAllAsync('runtime-session');
      } finally {
        setDatabaseBindingOverride(previous);
      }
    });

    it('ships the same table as a migration of the datasource it is given', () => {
      const [source] = sessionMigrations({ name: 'app_sessions', schema: 'app_sessions' }).migrationSources();
      expect([source.datasource, source.schema]).toEqual(['app_sessions', 'app_sessions']);
      expect(source.definitions.map((definition) => definition.sql)).toEqual([SESSION_TABLE_SQL]);
    });
  });
});
