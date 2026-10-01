import { computeBundleDigest, computePayloadHash, writeBundle } from '@putnami/migration';
import { describe, expect, it } from 'bun:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { sqlBundleContributions, SQLSource } from '../src/migrations';

describe('sqlBundleContributions', () => {
  it('maps SQL definitions to operations and payloads', () => {
    const source = new SQLSource({
      namespace: 'iam',
      datasource: { name: 'default', schema: 'public' },
      definitions: [
        {
          name: '0001_create_users',
          sql: 'CREATE TABLE iam.users (id uuid primary key);',
          down: 'DROP TABLE iam.users;',
        },
        { name: '0002_add_email', sql: 'ALTER TABLE iam.users ADD COLUMN email text;' },
      ],
    });

    const { operations, payloads } = sqlBundleContributions([source]);
    expect(operations).toHaveLength(2);

    const [create, addEmail] = operations;
    expect(create.name).toBe('iam/0001_create_users');
    expect(create.target).toBe('default');
    expect(create.schema).toBe('public');
    expect(create.up.path).toBe('payload/sql/default/iam/0001_create_users.up.sql');
    expect(create.up.hash).toBe(computePayloadHash('CREATE TABLE iam.users (id uuid primary key);'));
    expect(create.down?.path).toBe('payload/sql/default/iam/0001_create_users.down.sql');
    expect(create.capabilities).toEqual({ transactional: true, reversible: true });

    expect(addEmail.down).toBeUndefined();
    expect(addEmail.capabilities).toEqual({ transactional: true });

    // payloads carry the raw SQL for both up and the single down.
    expect(payloads).toHaveLength(3);
    expect(payloads.find((p) => p.path === create.up.path)?.bytes).toBe(
      'CREATE TABLE iam.users (id uuid primary key);',
    );
  });

  it('produces a bundle that writes and digests cleanly', async () => {
    const source = new SQLSource({
      namespace: 'iam',
      datasource: { name: 'analytics', schema: 'iam' },
      definitions: [{ name: '0001_init', sql: 'CREATE SCHEMA iam;' }],
    });
    const { operations, payloads } = sqlBundleContributions([source]);

    const dir = await mkdtemp(join(tmpdir(), 'db-bundle-'));
    try {
      const bundle = { appName: 'demo', operations };
      await writeBundle(dir, bundle, payloads);
      expect(computeBundleDigest(bundle)).toMatch(/^[a-f0-9]{64}$/);
      // analytics datasource flows into the payload path.
      expect(operations[0].up.path).toBe('payload/sql/analytics/iam/0001_init.up.sql');
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('carries the declared schema and omits it when absent', () => {
    // The schema travels so an applier reconstructs a schema-aware source; a
    // schema-less source omits it (mirroring Go's omitempty) so schema-less
    // bundles stay byte- and digest-identical across runtimes.
    const withSchema = new SQLSource({
      namespace: 'registry',
      datasource: { name: 'registry_put', schema: 'registry_put' },
      definitions: [{ name: '0001_blobs', sql: 'CREATE TABLE blobs (id int);' }],
    });
    const withoutSchema = new SQLSource({
      namespace: 'audit',
      datasource: { name: 'audit', schema: '' },
      definitions: [{ name: '0001_log', sql: 'CREATE TABLE log (id int);' }],
    });

    const { operations } = sqlBundleContributions([withSchema, withoutSchema]);
    expect(operations[0].schema).toBe('registry_put');
    expect(operations[1].schema).toBeUndefined();
    expect(Object.hasOwn(operations[1], 'schema')).toBe(false);
  });
});
