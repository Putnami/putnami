import { Optional } from '@putnami/runtime';
import { Column, Key, Table } from '../table';
import type { InferTable } from '../table/table-inference';

/**
 * Table definition for tracking executed migrations in the database.
 * Matches the canonical migration protocol state store.
 */
export const MigrationsTable = Table('migration.migrations', {
  id: Key(String),
  dbName: Column(String, { columnName: 'db_name' }),
  name: Column(String),
  hash: Column(String),
  executedAt: Column(String, { columnName: 'executed_at' }),
  executionTimeMs: Column(Number, { columnName: 'execution_time_ms' }),
  success: Column(Number),
  errorMessage: Column(Optional(String), { columnName: 'error_message' }),
  downSql: Column(Optional(String), { columnName: 'down_sql' }),
  downHash: Column(Optional(String), { columnName: 'down_hash' }),
});

/** Entity representing an executed migration in the database */
export type Migration = InferTable<typeof MigrationsTable> & { db?: string };

/** The canonical datasource-scoped migration identity, `${datasource}:${name}`. */
export { canonicalMigrationId } from '@putnami/migration';
