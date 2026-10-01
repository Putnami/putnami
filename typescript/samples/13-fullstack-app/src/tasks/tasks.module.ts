import { api, module, type Plugin } from '@putnami/application';
import { sqlSourceInline } from '@putnami/database';
import type { MigrationContributor } from '@putnami/migration';
import { tasksMigration } from './tasks.entity';

const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({
      namespace: 'tasks',
      datasource: { name: 'default', schema: 'public' },
      definitions: [tasksMigration],
    }),
  ],
};

export const tasks = () =>
  module('tasks')
    .feature({
      id: 'tasks/manage',
      name: 'Task management',
      outcome: 'Users can create, list, update and complete tasks',
      owner: 'samples',
    })
    .path('/api/tasks')
    .use(migrations)
    .use(api());
