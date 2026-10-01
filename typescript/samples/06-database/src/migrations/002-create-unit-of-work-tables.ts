// Tables for the unit-of-work + consume-once proof (see test/unit-of-work.test.ts).
export const migration = {
  name: '20260719120000_create_unit_of_work_tables',
  sql: `
    CREATE TABLE IF NOT EXISTS device_codes (
      code        TEXT PRIMARY KEY,
      consumed    BOOLEAN NOT NULL DEFAULT false,
      consumed_by TEXT
    );

    CREATE TABLE IF NOT EXISTS signing_keys (
      id     TEXT PRIMARY KEY,
      tenant TEXT NOT NULL,
      state  TEXT NOT NULL
    );

    CREATE TABLE IF NOT EXISTS key_bindings (
      key_id TEXT PRIMARY KEY,
      tenant TEXT NOT NULL UNIQUE
    );
  `,
  down: `
    DROP TABLE IF EXISTS device_codes;
    DROP TABLE IF EXISTS key_bindings;
    DROP TABLE IF EXISTS signing_keys;
  `,
};
