import { describe, expect, it } from 'bun:test';
import { isMigrationBundleContributor } from '@putnami/migration';
import { sqlSourceInline, SQLSource } from '../src/migrations';

describe('SQLSource.infraDatabase', () => {
  it('maps datasource → database name and the declared schema, engine postgres', () => {
    // The schema is the declared one ("identity"), NOT the namespace ("iam") —
    // decoupling identity from schema is the whole point.
    const source = new SQLSource({
      namespace: 'iam',
      datasource: { name: 'primary', schema: 'identity' },
      definitions: [],
    });
    expect(source.infraDatabase()).toEqual({ name: 'primary', engine: 'postgres', schemas: ['identity'] });
  });

  it('defaults the database name to the default datasource', () => {
    const source = sqlSourceInline({ namespace: 'audit', datasource: { name: '', schema: 'audit' }, definitions: [] });
    expect(source.infraDatabase()).toEqual({ name: 'default', engine: 'postgres', schemas: ['audit'] });
  });

  it('omits schemas when the source declares none', () => {
    const source = sqlSourceInline({
      namespace: 'audit',
      datasource: { name: 'default', schema: '' },
      definitions: [],
    });
    expect(source.infraDatabase()).toEqual({ name: 'default', engine: 'postgres' });
  });
});

describe('SQLSource as a MigrationBundleContributor', () => {
  it('contributes bundle operations on its own, independent of a runner', () => {
    const source = new SQLSource({
      namespace: 'iam',
      datasource: { name: 'default', schema: 'public' },
      definitions: [{ name: '0001_init', sql: 'CREATE TABLE iam_users (id uuid primary key);' }],
    });
    expect(isMigrationBundleContributor(source)).toBe(true);

    const { operations, payloads } = source.migrationBundleOperations();
    expect(operations).toHaveLength(1);
    expect(operations[0].name).toBe('iam/0001_init');
    expect(operations[0].target).toBe('default');
    expect(payloads).toHaveLength(1);
  });
});
