import {
  type AsyncSessionStore,
  type Plugin,
  registerSessionStore,
  removeRequestSession,
  requestSessionData,
  SessionConfig,
  writeRequestSession,
} from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { abortableQuery } from '../abort';
import { database } from '../factory';
import { type Datasource, type SQLDefinition, type SQLSource, sqlSourceInline } from '../migrations/sql-source';

/** The DDL of the `sessions` table, shared by {@link DatabaseSessionStore} and {@link sessionTableMigrations}. */
export const SESSION_TABLE_SQL = `CREATE TABLE IF NOT EXISTS sessions (
  session_id VARCHAR(255) PRIMARY KEY,
  data JSONB NOT NULL DEFAULT '{}',
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at);`;

/** The migration that creates the `sessions` table. */
export const sessionTableMigrations: readonly SQLDefinition[] = [
  {
    name: '20261115000000_create_sessions',
    sql: SESSION_TABLE_SQL,
    down: 'DROP TABLE IF EXISTS sessions;',
  },
];

/**
 * A plugin that ships the `sessions` table as a migration of `datasource`.
 *
 * Mount it next to `sql({ datasource })` in a workload whose runtime role
 * cannot create tables: the migration creates the table at deploy, and
 * {@link DatabaseSessionStore} then runs no DDL.
 */
export function sessionMigrations(datasource: Datasource): Plugin & { migrationSources(): SQLSource[] } {
  return {
    migrationSources() {
      return [sqlSourceInline({ namespace: 'sessions', datasource, definitions: sessionTableMigrations })];
    },
  };
}

/**
 * Database-backed session store using PostgreSQL.
 *
 * Stores session data in a `sessions` table with JSONB data column.
 * The table is created on first use when it does not exist. A workload whose
 * runtime role cannot create tables ships it with {@link sessionMigrations}.
 *
 * **Characteristics:**
 * - Persistent - survives server restarts
 * - Scalable - works with multi-instance deployments
 * - Queryable - can analyze sessions via SQL
 * - Automatic TTL expiration
 *
 * **Database Schema:**
 * ```sql
 * CREATE TABLE sessions (
 *   session_id VARCHAR(255) PRIMARY KEY,
 *   data JSONB NOT NULL DEFAULT '{}',
 *   expires_at TIMESTAMPTZ NOT NULL,
 *   created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 *   updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
 * )
 * ```
 *
 * The session API (`useSession`, `setSession`, ...) works unchanged: the HTTP
 * plugin loads the row once before the middlewares and writes it once after
 * the response, only when the request changed the session. The async methods
 * (`getAllAsync`, `setAllAsync`, ...) read and write the table directly.
 *
 * @example Configuration
 * ```yaml
 * session:
 *   store: 'database'
 *   ttl: 604800  # 1 week
 * database:
 *   host: 'localhost'
 *   database: 'myapp'
 * ```
 */
export class DatabaseSessionStore implements AsyncSessionStore {
  readonly asyncSession = true as const;
  private initPromise?: Promise<void>;
  private dbName?: string;

  constructor(dbName?: string) {
    this.dbName = dbName;
  }

  async ensureTable(): Promise<void> {
    if (!this.initPromise) {
      this.initPromise = this.initTable().catch((err) => {
        // Reset so next call retries on transient failure
        this.initPromise = undefined;
        throw err;
      });
    }
    return this.initPromise;
  }

  private async initTable(): Promise<void> {
    const sql = await database(this.dbName);
    // A deployed workload's runtime role may read and write rows but not
    // create tables, and PostgreSQL checks that right before it checks
    // IF NOT EXISTS. When a migration already created the table, run no DDL.
    const [{ present }] = await sql<{ present: boolean }[]>`SELECT to_regclass('sessions') IS NOT NULL AS present`;
    if (present) return;
    await sql.unsafe(SESSION_TABLE_SQL);
  }

  get<T>(sessionId: string, key: string): T | undefined {
    return this.getAll<Record<string, unknown>>(sessionId)[key] as T | undefined;
  }

  set<T>(sessionId: string, key: string, value: T): void {
    this.setAll(sessionId, { ...this.getAll<Record<string, unknown>>(sessionId), [key]: value });
  }

  delete(sessionId: string, key: string): void {
    const session = this.getAll<Record<string, unknown>>(sessionId);
    if (!(key in session)) return;
    delete session[key];
    this.setAll(sessionId, session);
  }

  /** The request's copy of the session, loaded by the HTTP plugin before the middlewares. */
  getAll<S = Record<string, unknown>>(_sessionId: string): S {
    return requestSessionData<S>();
  }

  /** Replaces the request's copy; the HTTP plugin writes it once the response is built. */
  setAll<S>(sessionId: string, session: S): void {
    writeRequestSession(sessionId, session as Record<string, unknown>);
  }

  /** Empties the request's copy; the HTTP plugin deletes the row once the response is built. */
  deleteAll(sessionId: string): void {
    removeRequestSession(sessionId);
  }

  exists(sessionId: string): boolean {
    return Object.keys(this.getAll(sessionId)).length > 0;
  }

  loadAsync(sessionId: string): Promise<Record<string, unknown>> {
    return this.getAllAsync<Record<string, unknown>>(sessionId);
  }

  saveAsync(sessionId: string, data: Record<string, unknown>): Promise<void> {
    return this.setAllAsync(sessionId, data);
  }

  removeAsync(sessionId: string): Promise<void> {
    return this.deleteAllAsync(sessionId);
  }

  // Async implementations for actual DB operations

  async getAsync<T>(sessionId: string, key: string): Promise<T | undefined> {
    const session = await this.getAllAsync<Record<string, unknown>>(sessionId);
    return session?.[key] as T | undefined;
  }

  async getAllAsync<S = Record<string, unknown>>(sessionId: string): Promise<S> {
    await this.ensureTable();
    const sql = await database(this.dbName);
    const config = useConfig(SessionConfig);
    const expiresAt = new Date(Date.now() + config.ttl * 1000);

    // Single query: extend TTL and return data atomically. The data comes
    // back as text and is parsed here, so the session returns exactly the
    // keys it was given (`oauth_pending` stays `oauth_pending`).
    const result = await abortableQuery(sql<{ data: string }[]>`
      UPDATE sessions
      SET expires_at = ${expiresAt}, updated_at = NOW()
      WHERE session_id = ${sessionId}
        AND expires_at > NOW()
      RETURNING data::text AS data
    `);

    if (result.length === 0) {
      return {} as S;
    }

    return JSON.parse(result[0].data) as S;
  }

  async setAllAsync<S>(sessionId: string, session: S): Promise<void> {
    await this.ensureTable();
    const sql = await database(this.dbName);
    const config = useConfig(SessionConfig);
    const expiresAt = new Date(Date.now() + config.ttl * 1000);

    // Cast session to unknown then to JSONValue-compatible type for sql.json()
    const sessionData = session as unknown as Parameters<typeof sql.json>[0];

    await abortableQuery(sql`
      INSERT INTO sessions (session_id, data, expires_at)
      VALUES (${sessionId}, ${sql.json(sessionData)}, ${expiresAt})
      ON CONFLICT (session_id) DO UPDATE
      SET data = ${sql.json(sessionData)},
          expires_at = ${expiresAt},
          updated_at = NOW()
    `);
  }

  async deleteAllAsync(sessionId: string): Promise<void> {
    await this.ensureTable();
    const sql = await database(this.dbName);
    await abortableQuery(sql`DELETE FROM sessions WHERE session_id = ${sessionId}`);
  }

  async existsAsync(sessionId: string): Promise<boolean> {
    await this.ensureTable();
    const sql = await database(this.dbName);
    const result = await abortableQuery(sql`
      SELECT 1 FROM sessions
      WHERE session_id = ${sessionId}
        AND expires_at > NOW()
    `);
    return result.length > 0;
  }

  /** Cleanup expired sessions */
  async cleanup(): Promise<number> {
    await this.ensureTable();
    const sql = await database(this.dbName);
    const result = await sql`
      DELETE FROM sessions WHERE expires_at < NOW()
    `;
    return result.count;
  }
}

// Auto-register on import
registerSessionStore('database', () => new DatabaseSessionStore());
