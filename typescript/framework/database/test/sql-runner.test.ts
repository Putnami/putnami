import { describe, expect, it } from 'bun:test';
import { MigrationRegistry } from '@putnami/migration';
import { specTest } from '@putnami/spectest';
import { SQLRunner, SQLSource } from '../src/migrations';

// The schema → search_path threading is decided in SQLRunner.materialize(),
// which runs before any connection is opened, so these assertions need no
// database. materialize() is private; reach it through a structural cast.
type Materializable = { materialize(): Map<string, { schema: string }> };
const materialize = (runner: SQLRunner) => (runner as unknown as Materializable).materialize();

const source = (namespace: string, name: string, schema: string, sql = 'SELECT 1') =>
  new SQLSource({ namespace, datasource: { name, schema }, definitions: [{ name: '001', sql }] });

describe('SQLRunner schema → search_path threading', () => {
  specTest(
    "threads each datasource's declared schema onto its Migrator",
    {
      feature: 'typescript/sql-migrations',
      requirement: 'schema-scoped-ddl',
      check: 'each-datasource-schema-threads-onto-its-migrator',
    },
    () => {
      // One migration shape registered against several datasources, each owning
      // its own schema — the "one migration set → many schemas" arrangement.
      const registry = new MigrationRegistry();
      registry.addSource(source('registry', 'registry_put', 'registry_put'));
      registry.addSource(source('registry', 'registry_oci', 'registry_oci'));

      const migrators = materialize(new SQLRunner({ registry, autoApply: true }));
      expect(migrators.get('registry_put')?.schema).toBe('registry_put');
      expect(migrators.get('registry_oci')?.schema).toBe('registry_oci');
    },
  );

  it('accepts the same datasource re-declaring the same schema across sources', () => {
    const registry = new MigrationRegistry();
    registry.addSource(source('a', 'shared', 'identity', 'SELECT 1'));
    registry.addSource(
      new SQLSource({
        namespace: 'b',
        datasource: { name: 'shared', schema: 'identity' },
        definitions: [{ name: '002', sql: 'SELECT 2' }],
      }),
    );

    const migrators = materialize(new SQLRunner({ registry, autoApply: true }));
    expect(migrators.get('shared')?.schema).toBe('identity');
  });

  specTest(
    'rejects two sources mapping one datasource to different schemas',
    {
      feature: 'typescript/sql-migrations',
      requirement: 'schema-scoped-ddl',
      check: 'conflicting-schema-declarations-are-rejected',
    },
    () => {
      const registry = new MigrationRegistry();
      registry.addSource(source('a', 'shared', 'schema_a'));
      registry.addSource(source('b', 'shared', 'schema_b'));

      const runner = new SQLRunner({ registry, autoApply: true });
      expect(() => materialize(runner)).toThrow(
        'datasource "shared" has conflicting schemas across sources: "schema_a" and "schema_b" (namespaces "a" and "b")',
      );
    },
  );
});
