import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Absolute path to the real Table() builder so on-disk fixtures produce
// definitions collectTableDefinitions recognises (the brand is a plain string,
// stable across module instances).
const TABLE_PATH = join(import.meta.dir, '../src/table/index.ts');

let currentProject: { name: string } = { name: 'app' };
let projectRoot = '';
let dependencyGraph = new Map<string, string[]>();

const realRuntime = require('../../runtime/src/index');

mock.module('@putnami/runtime', () => realRuntime);

mock.module('@putnami/utils', () => {
  const real = require('../../utils/src/index');
  return {
    ...real,
    getCurrentProject: () => currentProject,
    getProjectRoot: () => projectRoot,
    listProjectDependencies: (name: string) => dependencyGraph.get(name) ?? [name],
  };
});

const { sql } = await import('../src/sql.plugin');

async function writeSource(relPath: string, contents: string) {
  const abs = join(projectRoot, relPath);
  await mkdir(join(abs, '..'), { recursive: true });
  await writeFile(abs, contents, 'utf8');
}

// A library dependency that only *exports* table definitions. It never runs the
// sql() plugin's generate(), so it emits no fragment of its own — the workload
// that depends on it must claim the database its tables need.
//
// Written as a real package under the workload's node_modules rather than a
// mock.module: emitInfraRequirements resolves each dependency from projectPath
// (the workload root, where an isolated-linker workspace lib is symlinked)
// before importing it, so the regression path only fires against an on-disk
// package that Bun.resolveSync can actually find from there.
async function writeDependencyPackage(name: string) {
  const pkgDir = join(projectRoot, 'node_modules', name);
  await mkdir(pkgDir, { recursive: true });
  await writeFile(join(pkgDir, 'package.json'), JSON.stringify({ name, version: '0.0.0', main: 'index.ts' }), 'utf8');
  await writeFile(
    join(pkgDir, 'index.ts'),
    [
      `import { Table } from ${JSON.stringify(TABLE_PATH)};`,
      "export const UsersTable = Table('users', {}, { schema: 'iam' });",
      "export const ApiKeysTable = Table('api_keys', {}, { schema: 'iam' });",
      "export const notATable = () => 'helper';",
      '',
    ].join('\n'),
    'utf8',
  );
}

async function readSidecar() {
  return JSON.parse(await readFile(join(projectRoot, '.gen/infra/database.json'), 'utf8'));
}

beforeEach(async () => {
  projectRoot = await mkdtemp(join(tmpdir(), 'putnami-sql-gen-'));
  currentProject = { name: 'app' };
  dependencyGraph = new Map([['app', ['app']]]);
});

afterEach(async () => {
  await rm(projectRoot, { recursive: true, force: true });
});

describe('sql() generate() infra sidecar', () => {
  it('emits a sidecar for tables that live only in a dependency package', async () => {
    // The workload has no local Table() calls; every table lives in the
    // @acme/auth-core library — the thin-workload-around-libs case.
    // The library is a real package linked under the workload's node_modules so
    // the dependency import is resolved from projectPath, not this plugin's own
    // (isolated-store) module location — the regression guarded by the fix.
    await writeDependencyPackage('@acme/auth-core');
    dependencyGraph = new Map([
      ['app', ['app', '@putnami/database', '@acme/auth-core']],
      ['@acme/auth-core', ['@acme/auth-core', '@putnami/database']],
    ]);

    // Must not throw a "Cannot find module '@acme/auth-core'" resolution error.
    const result = await sql().generate?.({} as never);

    expect(result).toEqual({ exports: { 'sql-loader': join(projectRoot, '.gen/src/.sql.gen.ts') } });
    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      databases: [{ name: 'default', engine: 'postgres', schemas: ['iam'] }],
    });
  });

  it('merges tables from local files and dependency packages', async () => {
    await writeDependencyPackage('@acme/auth-core');
    dependencyGraph = new Map([
      ['app', ['app', '@putnami/database', '@acme/auth-core']],
      ['@acme/auth-core', ['@acme/auth-core', '@putnami/database']],
    ]);
    await writeSource(
      'src/tables.ts',
      [
        '// declares tables for @putnami/database',
        `import { Table } from ${JSON.stringify(TABLE_PATH)};`,
        "export const AccountsTable = Table('accounts', {}, { db: 'wealth', schema: 'ledger' });",
        '',
      ].join('\n'),
    );

    await sql().generate?.({} as never);

    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      databases: [
        { name: 'default', engine: 'postgres', schemas: ['iam'] },
        { name: 'wealth', engine: 'postgres', schemas: ['ledger'] },
      ],
    });
  });

  it('falls tables without db: back to the declared primary datasource (local files)', async () => {
    // sql({ datasource: 'identity' }) makes a db-less Table() emit its infra
    // requirement against 'identity' instead of the framework 'default' — so the
    // provisioned database matches the one the runtime reads/writes.
    await writeSource(
      'src/tables.ts',
      [
        '// declares tables for @putnami/database',
        `import { Table } from ${JSON.stringify(TABLE_PATH)};`,
        "export const UsersTable = Table('users', {}, { schema: 'identity_auth' });",
        '',
      ].join('\n'),
    );

    await sql({ datasource: 'identity' }).generate?.({} as never);

    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      databases: [{ name: 'identity', engine: 'postgres', schemas: ['identity_auth'] }],
    });
  });

  it('lets db-less library tables inherit the consuming workload primary datasource', async () => {
    // The library's tables set no db (they are datasource-agnostic); the workload
    // declaring sql({ datasource: 'identity' }) binds them — the inheritance the
    // runtime applies, mirrored at generate time.
    await writeDependencyPackage('@acme/auth-core');
    dependencyGraph = new Map([
      ['app', ['app', '@putnami/database', '@acme/auth-core']],
      ['@acme/auth-core', ['@acme/auth-core', '@putnami/database']],
    ]);

    await sql({ datasource: { name: 'identity', schema: 'identity_auth' } }).generate?.({} as never);

    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      databases: [{ name: 'identity', engine: 'postgres', schemas: ['iam'] }],
    });
  });

  it('leaves a table with an explicit db: on its own datasource', async () => {
    await writeSource(
      'src/tables.ts',
      [
        '// declares tables for @putnami/database',
        `import { Table } from ${JSON.stringify(TABLE_PATH)};`,
        "export const UsersTable = Table('users', {}, { schema: 'identity_auth' });",
        "export const LedgerTable = Table('ledger', {}, { db: 'wealth', schema: 'ledger' });",
        '',
      ].join('\n'),
    );

    await sql({ datasource: 'identity' }).generate?.({} as never);

    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      databases: [
        { name: 'identity', engine: 'postgres', schemas: ['identity_auth'] },
        { name: 'wealth', engine: 'postgres', schemas: ['ledger'] },
      ],
    });
  });

  it('removes a stale sidecar when no tables remain anywhere', async () => {
    // A sidecar left by a prior build must be cleared once the project (and its
    // dependencies) declare no tables, so the aggregator can't read a stale need.
    await mkdir(join(projectRoot, '.gen/infra'), { recursive: true });
    await writeFile(
      join(projectRoot, '.gen/infra/database.json'),
      JSON.stringify({ databases: [{ name: 'default', engine: 'postgres' }] }),
      'utf8',
    );
    dependencyGraph = new Map([['app', ['app']]]);

    const result = await sql().generate?.({} as never);

    expect(result).toEqual({});
    expect(await Bun.file(join(projectRoot, '.gen/infra/database.json')).exists()).toBe(false);
  });
});
