import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import {
  buildInfraRequirements,
  collectTableDefinitions,
  databasesFromTables,
  toInfraEngine,
  VALID_ENGINES,
} from '../src/infra';
import { Key, Table } from '../src/table';

const id = { id: Key(Uuid) };

describe('toInfraEngine', () => {
  it('accepts every closed v1 engine', () => {
    for (const engine of VALID_ENGINES) {
      expect(toInfraEngine(engine)).toBe(engine);
    }
  });

  it('raises a clear diagnostic for an unknown engine', () => {
    expect(() => toInfraEngine('oracle')).toThrow(/unsupported database engine "oracle"/);
    expect(() => toInfraEngine('oracle')).toThrow(/postgres, mysql, sqlite/);
  });
});

describe('databasesFromTables', () => {
  it('emits nothing for zero pools', () => {
    expect(databasesFromTables([])).toEqual([]);
  });

  it('emits one Postgres entry for a single pool with one schema', () => {
    const tables = [Table('users', id, { schema: 'iam' })];
    expect(databasesFromTables(tables)).toEqual([{ name: 'default', engine: 'postgres', schemas: ['iam'] }]);
  });

  it('omits schemas when no table declares one', () => {
    const tables = [Table('users', id)];
    expect(databasesFromTables(tables)).toEqual([{ name: 'default', engine: 'postgres' }]);
  });

  it('emits one entry per pool, sorted by name', () => {
    const tables = [
      Table('users', id, { db: 'wealth' }),
      Table('accounts', id, { db: 'auth' }),
      Table('sessions', id, { db: 'auth' }),
    ];
    expect(databasesFromTables(tables)).toEqual([
      { name: 'auth', engine: 'postgres' },
      { name: 'wealth', engine: 'postgres' },
    ]);
  });

  specTest(
    'unions schemas across a pool, deduped and sorted',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'declared-infrastructure',
      check: 'pool-schemas-union-deduped-and-sorted',
    },
    () => {
      const tables = [
        Table('users', id, { schema: 'iam' }),
        Table('roles', id, { schema: 'iam' }),
        Table('audit_log', id, { schema: 'audit' }),
      ];
      expect(databasesFromTables(tables)).toEqual([{ name: 'default', engine: 'postgres', schemas: ['audit', 'iam'] }]);
    },
  );
});

describe('buildInfraRequirements', () => {
  it('returns undefined when there are no pools', () => {
    expect(buildInfraRequirements([])).toBeUndefined();
  });

  specTest(
    'produces a v1 manifest with the published schema URL',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'declared-infrastructure',
      check: 'tables-derive-the-infra-requirements-sidecar',
    },
    () => {
      const manifest = buildInfraRequirements([Table('users', id, { schema: 'iam' })]);
      expect(manifest).toEqual({
        $schema: 'https://putnami.dev/schemas/putnami-infra.json',
        protocolVersion: 2,
        databases: [{ name: 'default', engine: 'postgres', schemas: ['iam'] }],
      });
    },
  );
});

describe('collectTableDefinitions', () => {
  it('picks out only the exported table definitions', () => {
    const module = {
      UsersTable: Table('users', id),
      notATable: { foo: 'bar' },
      helper: () => 1,
    };
    expect(collectTableDefinitions(module)).toEqual([module.UsersTable]);
  });

  it('returns an empty list for a module with no tables', () => {
    expect(collectTableDefinitions({ foo: 1 })).toEqual([]);
  });
});
