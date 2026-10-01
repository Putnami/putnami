import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { Migrator } from '../src/index';
import { database } from '../src/factory';
import { MigrationError } from '../src/errors';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

const DB_NAME = 'migrator-atomic-test';
const dockerAvailable = await isDockerAvailable();

// Name of the migration the failing-bookkeeping trigger targets.
const TRIP_NAME = 'migrator-atomic-test/create-atomic-tripwire';

describe.skipIf(!dockerAvailable)('Migrator up-path atomicity', () => {
  let sql: Awaited<ReturnType<typeof database>>;

  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);
    sql = await database(DB_NAME);
    // Ensure the bookkeeping schema/table exist (a Migrator creates them
    // lazily, but the trigger below and the assertions query them directly).
    await sql`CREATE SCHEMA IF NOT EXISTS migration`;
    await sql`CREATE TABLE IF NOT EXISTS migration.migrations (
      id TEXT PRIMARY KEY, db_name TEXT NOT NULL, name TEXT NOT NULL,
      hash TEXT NOT NULL, executed_at TEXT NOT NULL, execution_time_ms INTEGER NOT NULL,
      success INTEGER NOT NULL, error_message TEXT, down_sql TEXT, down_hash TEXT
    )`;
  }, 30_000);

  beforeEach(async () => {
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DB_NAME}`;
    await sql`DROP TABLE IF EXISTS atomic_ok`;
    await sql`DROP TABLE IF EXISTS atomic_tripwire`;
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterEach(async () => {
    // Always remove the tripwire so a failed assertion can't poison siblings.
    await dropBookkeepingTripwire(sql);
  });

  afterAll(async () => {
    await dropBookkeepingTripwire(sql);
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DB_NAME}`;
    await sql`DROP TABLE IF EXISTS atomic_ok`;
    await sql`DROP TABLE IF EXISTS atomic_tripwire`;
  });

  it('records exactly one success row for a successful up-migration', async () => {
    const migrator = new Migrator(DB_NAME, [
      {
        name: 'migrator-atomic-test/create-atomic-ok',
        sql: `CREATE TABLE atomic_ok (id TEXT PRIMARY KEY);`,
      },
    ]);

    const applied = await migrator.up();
    expect(applied.length).toBe(1);

    const rows = await sql`
      SELECT * FROM migration.migrations
      WHERE db_name = ${DB_NAME} AND name = 'migrator-atomic-test/create-atomic-ok'
    `;
    expect(rows.length).toBe(1);
    expect(rows[0].success).toBe(1);

    const reg = await sql`SELECT to_regclass('atomic_ok') AS reg`;
    expect(reg[0].reg).not.toBeNull();
  });

  it('rolls back the applied DDL when the bookkeeping insert fails (atomic up-path)', async () => {
    // The partial-failure window: the up DDL succeeds, but the *separate*
    // bookkeeping INSERT fails (simulating a crash/timeout between the two).
    // A BEFORE INSERT trigger on migration.migrations rejects the row for
    // this one migration. With the atomic fix the DDL is rolled back with the
    // INSERT; without it the table commits but is never recorded.
    await installBookkeepingTripwire(sql);

    const migrator = new Migrator(DB_NAME, [
      {
        name: TRIP_NAME,
        sql: `CREATE TABLE atomic_tripwire (id TEXT PRIMARY KEY);`,
      },
    ]);

    let threw = false;
    try {
      await migrator.up();
    } catch (e) {
      threw = true;
      expect(e).toBeInstanceOf(MigrationError);
    }
    expect(threw).toBe(true);

    // The DDL must NOT have committed — it shares the bookkeeping insert's
    // transaction, so the failed INSERT rolls the CREATE TABLE back too.
    const tables = await sql`SELECT to_regclass('atomic_tripwire') AS reg`;
    expect(tables[0].reg).toBeNull();

    // And there must be no SUCCESS bookkeeping row; otherwise a re-run would
    // skip a migration whose effects were rolled back (broken idempotency).
    const successRows = await sql`
      SELECT * FROM migration.migrations
      WHERE db_name = ${DB_NAME} AND name = ${TRIP_NAME} AND success = 1
    `;
    expect(successRows.length).toBe(0);
  });

  it('records the failure row (success = 0) outside the rolled-back transaction', async () => {
    // The failure bookkeeping is written after ROLLBACK, so it survives even
    // though the up transaction was undone. The trigger only rejects the
    // success INSERT (success = 1), letting the failure INSERT through.
    await installBookkeepingTripwire(sql);

    const migrator = new Migrator(DB_NAME, [
      {
        name: TRIP_NAME,
        sql: `CREATE TABLE atomic_tripwire (id TEXT PRIMARY KEY);`,
      },
    ]);

    await expect(migrator.up()).rejects.toBeInstanceOf(MigrationError);

    const failureRows = await sql`
      SELECT * FROM migration.migrations
      WHERE db_name = ${DB_NAME} AND name = ${TRIP_NAME} AND success = 0
    `;
    expect(failureRows.length).toBe(1);

    // fetchApplied ignores success = 0, so a subsequent up() retries the
    // migration cleanly rather than skipping it.
    const applied = await migrator.status();
    expect(applied.some((m) => m.name === TRIP_NAME && m.success === 1)).toBe(false);
  });
});

describe.skipIf(!dockerAvailable)('Migrator targets the declared schema via search_path', () => {
  const DS = 'migrator-schema-test';
  let sql: Awaited<ReturnType<typeof database>>;

  beforeAll(async () => {
    await ensurePostgresContainer(DS);
    sql = await database(DS);
    await sql`CREATE SCHEMA IF NOT EXISTS migration`;
    await sql`CREATE TABLE IF NOT EXISTS migration.migrations (
      id TEXT PRIMARY KEY, db_name TEXT NOT NULL, name TEXT NOT NULL,
      hash TEXT NOT NULL, executed_at TEXT NOT NULL, execution_time_ms INTEGER NOT NULL,
      success INTEGER NOT NULL, error_message TEXT, down_sql TEXT, down_hash TEXT
    )`;
  }, 30_000);

  beforeEach(async () => {
    await sql`DROP SCHEMA IF EXISTS tenant_a CASCADE`;
    await sql`DROP SCHEMA IF EXISTS tenant_b CASCADE`;
    await sql`CREATE SCHEMA tenant_a`;
    await sql`CREATE SCHEMA tenant_b`;
    await sql`DROP TABLE IF EXISTS public.widgets`;
    await sql`DROP TABLE IF EXISTS public.gadgets`;
    await sql`DROP TABLE IF EXISTS public.sprockets`;
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DS}`;
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await sql`DROP SCHEMA IF EXISTS tenant_a CASCADE`;
    await sql`DROP SCHEMA IF EXISTS tenant_b CASCADE`;
    await sql`DROP TABLE IF EXISTS public.widgets`;
    await sql`DROP TABLE IF EXISTS public.gadgets`;
    await sql`DROP TABLE IF EXISTS public.sprockets`;
    await sql`DELETE FROM migration.migrations WHERE db_name = ${DS}`;
  });

  it('routes the same unqualified migration shape into whichever schema each Migrator declares', async () => {
    // One migration "set" (unqualified CREATE TABLE), two schemas. The declared
    // schema, applied as the transaction-local search_path, decides where the
    // objects land — no schema names baked into the SQL.
    await new Migrator(DS, [{ name: 'app/0001_widgets', sql: 'CREATE TABLE widgets (id int);' }], 'tenant_a').up();
    await new Migrator(DS, [{ name: 'app/0002_gadgets', sql: 'CREATE TABLE gadgets (id int);' }], 'tenant_b').up();

    expect((await sql`SELECT to_regclass('tenant_a.widgets') AS reg`)[0].reg).not.toBeNull();
    expect((await sql`SELECT to_regclass('tenant_b.gadgets') AS reg`)[0].reg).not.toBeNull();
    // Neither leaked into the connection's default (public) schema.
    expect((await sql`SELECT to_regclass('public.widgets') AS reg`)[0].reg).toBeNull();
    expect((await sql`SELECT to_regclass('public.gadgets') AS reg`)[0].reg).toBeNull();
  });

  it('leaves the connection search_path in force when no schema is declared', async () => {
    await new Migrator(DS, [{ name: 'app/0003_sprockets', sql: 'CREATE TABLE sprockets (id int);' }]).up();
    // No declared schema → the runner sets no search_path, so the body lands in
    // the connection's default (public) schema.
    expect((await sql`SELECT to_regclass('public.sprockets') AS reg`)[0].reg).not.toBeNull();
  });
});

/**
 * Install a BEFORE INSERT trigger on migration.migrations that raises an
 * exception when a SUCCESS row (success = 1) for the tripwire migration is
 * inserted. This makes the bookkeeping insert fail while the up DDL itself
 * succeeds — exactly the partial-failure window the atomic up-path closes.
 */
async function installBookkeepingTripwire(sql: Awaited<ReturnType<typeof database>>): Promise<void> {
  await sql.unsafe(`
    CREATE OR REPLACE FUNCTION migration._tripwire() RETURNS trigger AS $$
    BEGIN
      IF NEW.name = '${TRIP_NAME}' AND NEW.success = 1 THEN
        RAISE EXCEPTION 'tripwire: bookkeeping insert rejected';
      END IF;
      RETURN NEW;
    END;
    $$ LANGUAGE plpgsql;
  `);
  await sql.unsafe(`DROP TRIGGER IF EXISTS _tripwire ON migration.migrations;`);
  await sql.unsafe(`
    CREATE TRIGGER _tripwire BEFORE INSERT ON migration.migrations
    FOR EACH ROW EXECUTE FUNCTION migration._tripwire();
  `);
}

async function dropBookkeepingTripwire(sql: Awaited<ReturnType<typeof database>>): Promise<void> {
  await sql.unsafe(`DROP TRIGGER IF EXISTS _tripwire ON migration.migrations;`).catch(() => {});
  await sql.unsafe(`DROP FUNCTION IF EXISTS migration._tripwire();`).catch(() => {});
}
