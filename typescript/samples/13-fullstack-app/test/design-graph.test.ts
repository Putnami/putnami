import { describe, expect, it } from 'bun:test';
import { join } from 'node:path';
import { api, buildDesignGraph, module, type Plugin } from '@putnami/application';
import { provideRepository, repositoryToken, sqlSourceInline } from '@putnami/database';
import { events } from '@putnami/events';
import type { MigrationContributor } from '@putnami/migration';
import syncProjectStatus from '../src/events/task-status.sync-project.on';
import { TaskService } from '../src/tasks';
import { POST as createTask } from '../src/tasks/api/post';
import { Tasks, tasksMigration } from '../src/tasks/tasks.entity';

type MigrationPlacement = 'feature' | 'child' | 'none';

const REPOSITORY_ID = "service:named('putnami.repository:default.tasks')";

function taskFeature(placement: MigrationPlacement) {
  const migrations: Plugin & MigrationContributor = {
    migrationSources: () => [
      sqlSourceInline({
        namespace: 'tasks',
        datasource: { name: 'default', schema: 'public' },
        definitions: [tasksMigration],
      }),
    ],
  };
  const taskApi = api({ autoScan: false, prefix: '/api/tasks' }).register('/', { POST: createTask }, 'POST');
  const taskEvents = events({ autoScan: false, handlers: [syncProjectStatus] });
  const feature = module('tasks')
    .feature({
      id: 'tasks/manage',
      name: 'Task management',
      outcome: 'Users can create, list, update and complete tasks',
      owner: 'samples',
    })
    .use(taskApi)
    .use(taskEvents)
    .register(provideRepository(Tasks))
    .provide(TaskService, { deps: [repositoryToken(Tasks)] });

  if (placement === 'feature') feature.use(migrations);
  if (placement === 'child') feature.use(module('storage').use(migrations));
  return module('app').use(feature);
}

async function graphFor(placement: MigrationPlacement) {
  return buildDesignGraph(taskFeature(placement), '@example/13-fullstack-app', join(import.meta.dir, '..'));
}

describe('task feature design conformance', () => {
  it('tracks native API, event, and data declarations through add, move, and remove', async () => {
    const migrationId = 'data.migration:sql:tasks/002_create_tasks';
    const schemaId = 'data.schema:default:public';
    const baseline = await graphFor('feature');
    const kinds = new Set(baseline?.nodes.map(({ kind }) => kind));
    expect([...kinds]).toEqual(
      expect.arrayContaining([
        'api.operation',
        'api.schema',
        'service',
        'event.topic',
        'event.handler',
        'data.migration',
        'data.schema',
      ]),
    );
    expect(baseline?.edges).toContainEqual({
      from: 'module:@example/13-fullstack-app/tasks',
      to: migrationId,
      kind: 'contains',
      authority: 'exact',
    });
    expect(baseline?.edges).toContainEqual({
      from: migrationId,
      to: schemaId,
      kind: 'writes',
      authority: 'exact',
    });

    const moved = await graphFor('child');
    expect(moved?.edges).not.toContainEqual({
      from: 'module:@example/13-fullstack-app/tasks',
      to: migrationId,
      kind: 'contains',
      authority: 'exact',
    });
    expect(moved?.edges).toContainEqual({
      from: 'module:@example/13-fullstack-app/tasks/storage',
      to: migrationId,
      kind: 'contains',
      authority: 'exact',
    });
    // Moving the owner changes containment only: the write target is a fact of
    // the source itself, so it must survive the move unchanged.
    expect(moved?.edges).toContainEqual({
      from: migrationId,
      to: schemaId,
      kind: 'writes',
      authority: 'exact',
    });

    const removed = await graphFor('none');
    expect(removed?.nodes.some(({ id }) => id === migrationId || id === schemaId)).toBe(false);
    expect(removed?.edges.some(({ from, to }) => from === migrationId || to === migrationId)).toBe(false);
    expect(removed?.edges.some(({ from, to }) => from === schemaId || to === schemaId)).toBe(false);
  });

  it('connects the operation to the service and the service to its repository', async () => {
    const graph = await graphFor('feature');

    // The endpoint declares `.inject({ taskService: TaskService })` and the
    // module declares `provide(TaskService, { deps: [repositoryToken(Tasks)] })`.
    // Both are runtime declarations, so the whole chain from the exposed
    // operation to the repository identity is exact.
    expect(graph?.edges).toContainEqual({
      from: 'api.operation:POST:/api/tasks',
      to: 'service:TaskService',
      kind: 'injects',
      authority: 'exact',
    });
    expect(graph?.edges).toContainEqual({
      from: 'service:TaskService',
      to: REPOSITORY_ID,
      kind: 'injects',
      authority: 'exact',
    });
    // `provideRepository(Tasks)` is registered on the feature module, so the
    // feature owns the repository.
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/13-fullstack-app/tasks',
      to: REPOSITORY_ID,
      kind: 'contains',
      authority: 'exact',
    });
    // One identity for a token that both an endpoint injection and a module
    // registration declare.
    expect(graph?.nodes.filter(({ id }) => id === REPOSITORY_ID)).toHaveLength(1);
  });

  it('leaves a relation only migration SQL creates unrepresented', async () => {
    const graph = await graphFor('feature');

    // The tasks migration also creates indexes and (in a real outbox feature)
    // could create tables no `Table()` declares. Nothing here parses that SQL,
    // so the only data relations in the graph are declared ones — and this
    // fixture declares none through the sql plugin, which never ran.
    expect(graph?.nodes.some(({ kind }) => kind === 'data.table')).toBe(false);
    expect(graph?.nodes.filter(({ kind }) => kind === 'data.schema').map(({ id }) => id)).toEqual([
      'data.schema:default:public',
    ]);
  });
});
