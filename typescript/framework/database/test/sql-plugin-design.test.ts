import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { mkdir, mkdtemp, realpath, rm, writeFile } from 'node:fs/promises';
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

const { buildDesignGraph, module: appModule, serializeDesignGraph } = await import('@putnami/application');
const { sql } = await import('../src/sql.plugin');
const { provideRepository, repositoryToken } = await import('../src/repository/repository-di');
const { sqlSourceInline } = await import('../src/migrations');
const { Repository } = await import('../src/repository/repository');
import type { Plugin } from '@putnami/application';
import type { MigrationContributor } from '@putnami/migration';
import type { TableDefinition } from '../src/table';

const TASKS_SQL = [
  'CREATE TABLE IF NOT EXISTS tasks (id UUID PRIMARY KEY, title TEXT NOT NULL);',
  // A relation created only by migration SQL. No Table() declares it, so the
  // graph must leave it unrepresented rather than parse it out of the text.
  'CREATE TABLE IF NOT EXISTS task_outbox (id UUID PRIMARY KEY, payload JSONB NOT NULL);',
].join('\n');

async function writeTableFile(relPath: string, declarations: string): Promise<Record<string, unknown>> {
  const abs = join(projectRoot, relPath);
  await mkdir(join(abs, '..'), { recursive: true });
  await writeFile(
    abs,
    [
      '// declares tables for @putnami/database',
      `import { Column, Key, Table } from ${JSON.stringify(TABLE_PATH)};`,
      declarations,
      '',
    ].join('\n'),
    'utf8',
  );
  return (await import(abs)) as Record<string, unknown>;
}

function taskMigrations(): Plugin & MigrationContributor {
  return {
    migrationSources: () => [
      sqlSourceInline({
        namespace: 'tasks',
        datasource: { name: 'default', schema: 'public' },
        definitions: [{ name: '001_create_tasks', sql: TASKS_SQL }],
      }),
    ],
  };
}

const FEATURE = {
  id: 'tasks/manage',
  name: 'Task management',
  outcome: 'Users can create, list, update and complete tasks',
  owner: 'samples',
} as const;

beforeEach(async () => {
  projectRoot = await realpath(await mkdtemp(join(tmpdir(), 'putnami-sql-design-')));
  currentProject = { name: 'app' };
  dependencyGraph = new Map([['app', ['app']]]);
});

afterEach(async () => {
  await rm(projectRoot, { recursive: true, force: true });
});

describe('sql() contributeDesign', () => {
  // The feature-scoped regression fixture: one feature owns a service, a
  // repository, a table, a schema, and a migration, and every relationship in
  // it is read from a native declaration.
  it('connects a feature-scoped service to its repository, table, schema, and migration', async () => {
    const tables = await writeTableFile(
      'src/tasks/tasks.entity.ts',
      "export const Tasks = Table('tasks', { id: Key(String), title: Column(String) }, { db: 'default', schema: 'public' });",
    );
    const Tasks = tables['Tasks'] as TableDefinition;
    class TaskService {
      constructor(readonly tasks: InstanceType<typeof Repository>) {}
    }

    const plugin = sql();
    await plugin.generate?.({} as never);
    const application = appModule('app').use(
      appModule('tasks')
        .feature(FEATURE)
        .use(plugin)
        .use(taskMigrations())
        .register(provideRepository(Tasks))
        .provide(TaskService, { deps: [repositoryToken(Tasks)] }),
    );

    const graph = await buildDesignGraph(application, '@example/tasks', projectRoot);

    const repositoryId = "service:named('putnami.repository:default.tasks')";
    expect(graph?.nodes.map(({ id }) => id)).toEqual([
      'data.migration:sql:tasks/001_create_tasks',
      'data.schema:default:public',
      'data.table:default:public:tasks',
      'feature:tasks/manage',
      'infra:database:default',
      'lifecycle:configure:module:@example/tasks/tasks',
      'lifecycle:generate:module:@example/tasks/tasks',
      'module:@example/tasks/tasks',
      'service:TaskService',
      repositoryId,
    ]);
    // The repository is a plain injectable identity, byte-identical to the one
    // an endpoint injecting the same token mints. Enriching it here would make
    // the two declarations conflict and fail the build.
    expect(graph?.nodes.find(({ id }) => id === repositoryId)).toEqual({
      id: repositoryId,
      kind: 'service',
      name: "named('putnami.repository:default.tasks')",
    });
    expect(graph?.nodes.find(({ id }) => id === 'data.schema:default:public')).toEqual({
      id: 'data.schema:default:public',
      kind: 'data.schema',
      name: 'public',
      properties: { datasource: 'default', engine: 'postgres', schema: 'public' },
    });

    expect(graph?.edges).toEqual([
      {
        from: 'data.migration:sql:tasks/001_create_tasks',
        to: 'data.schema:default:public',
        kind: 'writes',
        authority: 'exact',
      },
      {
        from: 'data.schema:default:public',
        to: 'data.table:default:public:tasks',
        kind: 'contains',
        authority: 'exact',
      },
      { from: 'feature:tasks/manage', to: 'module:@example/tasks/tasks', kind: 'implementedBy', authority: 'exact' },
      {
        from: 'module:@example/tasks/tasks',
        to: 'data.migration:sql:tasks/001_create_tasks',
        kind: 'contains',
        authority: 'exact',
      },
      {
        from: 'module:@example/tasks/tasks',
        to: 'infra:database:default',
        kind: 'contains',
        authority: 'exact',
      },
      {
        from: 'module:@example/tasks/tasks',
        to: 'lifecycle:configure:module:@example/tasks/tasks',
        kind: 'contains',
        authority: 'exact',
      },
      {
        from: 'module:@example/tasks/tasks',
        to: 'lifecycle:generate:module:@example/tasks/tasks',
        kind: 'contains',
        authority: 'exact',
      },
      { from: 'module:@example/tasks/tasks', to: 'service:TaskService', kind: 'contains', authority: 'exact' },
      { from: 'module:@example/tasks/tasks', to: repositoryId, kind: 'contains', authority: 'exact' },
      { from: 'service:TaskService', to: repositoryId, kind: 'injects', authority: 'exact' },
      { from: repositoryId, to: 'data.table:default:public:tasks', kind: 'reads', authority: 'exact' },
      { from: repositoryId, to: 'data.table:default:public:tasks', kind: 'writes', authority: 'exact' },
    ]);
  });

  it('leaves a relation only migration SQL creates unrepresented', async () => {
    await writeTableFile(
      'src/tasks/tasks.entity.ts',
      "export const Tasks = Table('tasks', { id: Key(String) }, { db: 'default', schema: 'public' });",
    );
    const plugin = sql();
    await plugin.generate?.({} as never);
    const application = appModule('app').use(appModule('tasks').feature(FEATURE).use(plugin).use(taskMigrations()));

    const graph = await buildDesignGraph(application, '@example/tasks', projectRoot);

    // The migration's SQL creates task_outbox. Reading a CREATE TABLE out of
    // the text would be a source-text heuristic promoted to a native fact, so
    // only the declared `tasks` relation exists.
    expect(graph?.nodes.filter(({ kind }) => kind === 'data.table').map(({ id }) => id)).toEqual([
      'data.table:default:public:tasks',
    ]);
    expect(serializeDesignGraph(graph!)).not.toContain('outbox');
  });

  it('projects no repository for a table no declaration named one', async () => {
    await writeTableFile(
      'src/tasks/tasks.entity.ts',
      "export const Tasks = Table('tasks', { id: Key(String) }, { db: 'default' });",
    );
    const plugin = sql();
    await plugin.generate?.({} as never);
    const application = appModule('app').use(appModule('tasks').feature(FEATURE).use(plugin));

    const graph = await buildDesignGraph(application, '@example/tasks', projectRoot);

    // A Table() declares a relation, not a gateway over it. Minting a
    // repository per table would invent a dependency the composition never
    // declared — and a `reads`/`writes` edge that no code can exercise.
    expect(graph?.nodes.some(({ kind }) => kind === 'service')).toBe(false);
    expect(graph?.edges.some(({ kind }) => kind === 'reads' || kind === 'writes')).toBe(false);
    // No `schema` on the table means the namespace is bound at deploy time, so
    // there is nothing native to name.
    expect(graph?.nodes.some(({ kind }) => kind === 'data.schema')).toBe(false);
  });

  it('keeps table provenance project-relative and the graph byte-stable across runs', async () => {
    const tables = await writeTableFile(
      'src/tasks/tasks.entity.ts',
      "export const Tasks = Table('tasks', { id: Key(String) }, { db: 'default' });",
    );
    const plugin = sql();
    await plugin.generate?.({} as never);
    const application = appModule('app').use(
      appModule('tasks')
        .feature(FEATURE)
        .use(plugin)
        .register(provideRepository(tables['Tasks'] as TableDefinition)),
    );

    const first = await buildDesignGraph(application, '@example/tasks', projectRoot);
    const second = await buildDesignGraph(application, '@example/tasks', projectRoot);

    expect(first?.nodes.find(({ id }) => id === 'data.table:default:tasks')?.provenance?.path).toBe(
      'src/tasks/tasks.entity.ts',
    );
    expect(serializeDesignGraph(second!)).toBe(serializeDesignGraph(first!));
    expect(serializeDesignGraph(first!)).not.toContain(projectRoot);
  });

  it('separates same-named tables that declare different schemas', async () => {
    const tables = await writeTableFile(
      'src/tenancy/audit.entity.ts',
      [
        "export const PublicAudit = Table('audit', { id: Key(String) }, { db: 'default', schema: 'public' });",
        "export const BillingAudit = Table('audit', { id: Key(String) }, { db: 'default', schema: 'billing' });",
      ].join('\n'),
    );
    const plugin = sql();
    await plugin.generate?.({} as never);
    const application = appModule('app').use(
      appModule('tenancy')
        .feature({ ...FEATURE, id: 'tenancy/audit' })
        .use(plugin)
        .register(provideRepository(tables['PublicAudit'] as TableDefinition)),
    );

    // A relation is unique only within its schema, so these are two relations.
    // One identity for both made their differing `schema` property a conflicting
    // native declaration, which addNode throws on — failing the whole build.
    const graph = await buildDesignGraph(application, '@example/tasks', projectRoot);

    expect(graph?.nodes.filter(({ kind }) => kind === 'data.table').map(({ id }) => id)).toEqual([
      'data.table:default:billing:audit',
      'data.table:default:public:audit',
    ]);
    expect(graph?.nodes.filter(({ kind }) => kind === 'data.schema').map(({ id }) => id)).toEqual([
      'data.schema:default:billing',
      'data.schema:default:public',
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'data.schema:default:billing',
      to: 'data.table:default:billing:audit',
      kind: 'contains',
      authority: 'exact',
    });
  });

  it('contributes from the application root, where sql() is normally registered', async () => {
    const tables = await writeTableFile(
      'src/tasks/tasks.entity.ts',
      "export const Tasks = Table('tasks', { id: Key(String) }, { db: 'default' });",
    );
    await writeTableFile(
      'src/tasks/archive.entity.ts',
      "export const ArchivedTasks = Table('archived_tasks', { id: Key(String) }, { db: 'default' });",
    );
    await writeTableFile(
      'src/audit/audit.entity.ts',
      "export const Audit = Table('audit', { id: Key(String) }, { db: 'default' });",
    );
    const plugin = sql();
    await plugin.generate?.({} as never);
    // `sql()` owns one runner and one primary datasource for the whole workload,
    // so it belongs on the root — which declares no feature. Scoping the
    // contributor to its owner dropped every table and repository relationship
    // for exactly the composition the docs and samples use.
    const application = appModule('app')
      .use(plugin)
      .register(provideRepository(tables['Tasks'] as TableDefinition))
      .use(
        appModule('tasks')
          .feature(FEATURE, { sources: ['src/tasks'] })
          .use(taskMigrations()),
      )
      .use(
        appModule('audit').feature(
          { ...FEATURE, id: 'audit/review', name: 'Audit review' },
          { sources: ['src/audit'] },
        ),
      );

    const graph = await buildDesignGraph(application, '@example/tasks', projectRoot);

    const repositoryId = "service:named('putnami.repository:default.tasks')";
    expect(graph?.nodes.map(({ id }) => id)).toContain('data.table:default:tasks');
    expect(graph?.nodes.map(({ id }) => id)).toContain('infra:database:default');
    expect(graph?.edges).toContainEqual({
      from: repositoryId,
      to: 'data.table:default:tasks',
      kind: 'reads',
      authority: 'exact',
    });
    expect(graph?.edges).toContainEqual({
      from: repositoryId,
      to: 'data.table:default:tasks',
      kind: 'writes',
      authority: 'exact',
    });
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/tasks/tasks',
      to: 'infra:database:default',
      kind: 'contains',
      authority: 'derived',
      properties: { association: 'selected' },
      // Both tables require `default`; one semantic edge is kept with the
      // canonical declaration source instead of conflicting by scan order.
      provenance: { path: 'src/tasks/archive.entity.ts' },
    });
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/tasks/audit',
      to: 'infra:database:default',
      kind: 'contains',
      authority: 'derived',
      properties: { association: 'selected' },
      provenance: { path: 'src/audit/audit.entity.ts' },
    });
  });
});
