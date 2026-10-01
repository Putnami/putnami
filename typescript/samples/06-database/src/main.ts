import { api, application, http, logger, platform, redirect, type Plugin } from '@putnami/application';
import { sql, sqlSourceInline } from '@putnami/database';
import type { MigrationContributor } from '@putnami/migration';
import { migration } from './migrations/001-create-users';
import { migration as unitOfWorkMigration } from './migrations/002-create-unit-of-work-tables';
import './tables/device-codes';
import './tables/key-bindings';
import './tables/signing-keys';
import './tables/users';

const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({
      namespace: 'app',
      datasource: { name: 'default', schema: 'public' },
      definitions: [migration, unitOfWorkMigration],
    }),
  ],
};

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/users')))
    .use(logger())
    .use(sql({ autoApply: true }))
    .use(migrations)
    .use(platform())
    .use(api());
